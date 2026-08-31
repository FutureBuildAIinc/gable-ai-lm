// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/depot"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// orderSource fetches confirmed orders for a date (satisfied by *gable.Client).
type orderSource interface {
	ListOrdersForDate(ctx context.Context, date string) ([]gable.Order, error)
}

// vehicleSource fetches the fleet (with capacities) for CVRP assignment
// (satisfied by *gable.Client).
type vehicleSource interface {
	ListVehicles(ctx context.Context) ([]gable.Vehicle, error)
}

// driverSource fetches the fleet's drivers; GableLBM requires a valid driver id
// on delivery-route write-back (satisfied by *gable.Client).
type driverSource interface {
	ListDrivers(ctx context.Context) ([]gable.Driver, error)
}

// locationSource fetches the dealer's yards, so a plan can be rooted at the
// branch it was asked for rather than at the middle of its own stops
// (satisfied by *gable.Client).
type locationSource interface {
	ListLocations(ctx context.Context) ([]gable.Location, error)
}

// routeSink writes an approved route back to GableLBM (satisfied by *gable.Client).
type routeSink interface {
	PushDeliveryRoute(ctx context.Context, route gable.DeliveryRoute) error
}

// planStore is the persistence seam for route plans (satisfied by
// *Repository). It is declared consumer-side like every other seam in this
// module so planning can be exercised against an in-memory store with no
// Postgres.
type planStore interface {
	Save(ctx context.Context, p *Plan) error
	Get(ctx context.Context, id string) (*Plan, error)
	UpdateStatus(ctx context.Context, id, status string) error
}

// Config carries this module's deployment configuration: the install-wide
// fallback yard (DEPOT_LAT/DEPOT_LNG). It is the same pair the workflow module
// is given, because both root their runs through the same ladder and an
// endpoint that disagreed with its neighbour about where the yard is would be
// the bug this file was changed to fix.
type Config struct {
	DepotLat *float64
	DepotLng *float64
}

// Service orchestrates route planning and write-back.
type Service struct {
	repo      planStore
	orders    orderSource
	vehicles  vehicleSource
	drivers   driverSource
	locations locationSource
	sink      routeSink
	cfg       Config
}

func NewService(repo planStore, orders orderSource, vehicles vehicleSource, drivers driverSource, locations locationSource, sink routeSink, cfg Config) *Service {
	return &Service{repo: repo, orders: orders, vehicles: vehicles, drivers: drivers, locations: locations, sink: sink, cfg: cfg}
}

// Plan pulls confirmed orders for the date, optimizes the stop sequence, and
// persists a DRAFT plan for dispatcher fine-tuning.
func (s *Service) Plan(ctx context.Context, req PlanRequest) (*Plan, error) {
	if req.Date == "" {
		return nil, fmt.Errorf("date is required")
	}

	orders, err := s.orders.ListOrdersForDate(ctx, req.Date)
	if err != nil {
		return nil, fmt.Errorf("fetch orders: %w", err)
	}

	// Build stops from orders that carry geolocation.
	var stops []Stop
	var points []depot.Point
	for _, o := range orders {
		if o.Latitude == nil || o.Longitude == nil {
			continue
		}
		var weight float64
		for _, l := range o.Lines {
			weight += l.WeightLbs * l.Quantity
		}
		stops = append(stops, Stop{
			OrderID:   o.ID,
			Lat:       *o.Latitude,
			Lng:       *o.Longitude,
			Address:   o.Address,
			WeightLbs: weight,
		})
		points = append(points, depot.Point{Lat: *o.Latitude, Lng: *o.Longitude})
	}

	depotLat, depotLng, depotSource, depotNote := s.resolveDepot(ctx, req, orders, points)
	slog.Info("routing plan depot resolved",
		"date", req.Date, "depot_source", depotSource, "depot_lat", depotLat, "depot_lng", depotLng,
		"note", depotNote)

	// Pull the live fleet and bin-pack stops across trucks by capacity (CVRP).
	vehicles, err := s.vehicles.ListVehicles(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch vehicles: %w", err)
	}
	loads, unassigned := assignLoads(vehicles, stops)

	// GableLBM requires a valid driver id on write-back. Assign drivers to loads
	// round-robin (deterministic, ACTIVE first) so each load can be pushed.
	drivers, err := s.drivers.ListDrivers(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch drivers: %w", err)
	}
	assignDrivers(drivers, loads)

	// Sequence each load independently and aggregate the plan-level totals.
	flattened := []Stop{}
	var totalDistance, totalDuration float64
	for i := range loads {
		ordered, distance, duration := active.Sequence(ctx, depotLat, depotLng, loads[i].Stops)
		if ordered == nil {
			ordered = []Stop{}
		}
		loads[i].Stops = ordered
		loads[i].TotalDistanceMi = distance
		loads[i].TotalDurationMin = duration
		flattened = append(flattened, ordered...)
		totalDistance += distance
		totalDuration += duration
	}
	if unassigned == nil {
		unassigned = []Stop{}
	}

	plan := &Plan{
		PlanDate:         req.Date,
		GableBranchID:    req.BranchID,
		GableVehicleID:   req.VehicleID,
		Loads:            loads,
		UnassignedStops:  unassigned,
		Stops:            flattened,
		TotalDistanceMi:  round2(totalDistance),
		TotalDurationMin: round2(totalDuration),
		Status:           "DRAFT",
	}
	if err := s.repo.Save(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// resolveDepot roots this plan through the shared ladder in internal/depot:
// REQUEST -> BRANCH -> CONFIG -> CENTROID -> NONE. It is the same ladder the
// workflow ingest uses, deliberately: this endpoint used to run its own shorter
// one (request depot, else the centroid of the stops) and so ignored the
// branch id its caller had explicitly handed it — it stored that branch on the
// plan and then rooted the route somewhere else.
//
// Sharing the ladder was not enough, though, because the two callers were still
// feeding it different questions. Workflow asked "which yards do these ORDERS
// ship from?"; routing asked only "which yard did the request name?" — so the
// same orders, planned through the two endpoints with no explicit branch, came
// out rooted at a yard in one and at the middle of the stops in the other, and
// the ladder's multi-yard guard could never fire here at all. The set handed in
// below is therefore derived from the orders, exactly as workflow derives it,
// through the same helper.
//
// req.BranchID keeps its meaning on top of that: it is the caller's own
// statement of which yard this plan is FOR, so it decides where the route is
// rooted. What it does not do is settle whether the dispatcher hears about a
// run whose stops are spread across yards — asking for Dallas does not move the
// Plano stops, and a plan that quietly routes half a day from the wrong yard is
// the failure this whole ladder exists to prevent. So an explicit branch
// overrides the origin and the span is still reported.
//
// The branch list is only fetched when it can actually change the answer: an
// explicit request depot outranks it, and a run naming no branch anywhere has
// nothing to look up. The fetch is best effort — a GableLBM that predates
// /api/integration/locations answers 404, and that must cost this plan its
// branch origin, not the plan itself.
//
// The result is not persisted: route_plans is a column-backed table and adding
// a depot_source column is a migration. The caller logs it instead, once per
// plan, so support can still answer "why does this route start there?".
func (s *Service) resolveDepot(ctx context.Context, req PlanRequest, orders []gable.Order, stops []depot.Point) (lat, lng float64, source, note string) {
	// The yards this day's orders actually ship from — the same question, asked
	// through the same helper, as internal/workflow asks of its analyses.
	orderBranches := depot.DistinctBranchIDs(orderBranchIDs(orders))

	explicit := ""
	if req.BranchID != nil {
		explicit = *req.BranchID
	}

	// An explicit branch is the caller's answer and outranks what the orders
	// imply; with none, the orders speak for themselves.
	wanted := orderBranches
	if explicit != "" {
		wanted = []string{explicit}
	}

	// A request depot outranks every branch, so with one there is nothing a
	// branch lookup could change — including the span sentence below, which is
	// held to the same rule the workflow ingest applies: an operator who gave
	// this run its own coordinate has already answered the question.
	branchRungInPlay := req.DepotLat == nil || req.DepotLng == nil

	var branches []gable.Location
	var branchesErr error
	if len(wanted) > 0 && branchRungInPlay {
		if branches, branchesErr = s.locations.ListLocations(ctx); branchesErr != nil {
			slog.Warn("could not list GableLBM branches; falling back down the depot chain",
				"date", req.Date, "branch_ids", wanted, "err", branchesErr)
			branches = nil
		}
	}

	lat, lng, source, note = depot.Resolve(depot.Input{
		RequestLat: req.DepotLat,
		RequestLng: req.DepotLng,
		BranchIDs:  wanted,
		Branches:   branches,
		ConfigLat:  s.cfg.DepotLat,
		ConfigLng:  s.cfg.DepotLng,
		Stops:      stops,
	})
	if branchesErr != nil {
		// A yard WAS named; we simply could not look it up. Say that, rather
		// than letting the note claim the branch was unknown.
		note = fmt.Sprintf("could not read GableLBM's branches (%v); %s", branchesErr, depot.FallbackPhrase(source))
	}
	if explicit != "" && branchRungInPlay {
		// The ladder only sees the one yard the request named, so it cannot
		// notice that the orders disagree. When they do, that fact still has to
		// reach the note: the plan is rooted where it was asked to be, and the
		// dispatcher is told which stops that leaves in another yard.
		if span := depot.SpanningBranchesPhrase(orderBranches, branches); span != "" {
			note = joinNotes(note, span+", and this plan is rooted at the branch named on the request ("+
				explicit+"), so the stops belonging to the other yards are routed from there too")
		}
	}
	return lat, lng, source, note
}

// orderBranchIDs projects the yard off each order. It is the routing module's
// half of the shared question — the semantics (first appearance, empties
// skipped, duplicates collapsed) belong to depot.DistinctBranchIDs, so this
// module cannot drift from workflow's answer again.
func orderBranchIDs(orders []gable.Order) []string {
	ids := make([]string, 0, len(orders))
	for _, o := range orders {
		ids = append(ids, o.BranchID)
	}
	return ids
}

// joinNotes appends a second sentence to a depot note without inventing a
// leading separator when the first one is empty.
func joinNotes(first, second string) string {
	if first == "" {
		return second
	}
	return first + "; " + second
}

// Get returns a stored plan by id.
func (s *Service) Get(ctx context.Context, id string) (*Plan, error) {
	return s.repo.Get(ctx, id)
}

// Approve marks a plan APPROVED and writes the route back to GableLBM.
func (s *Service) Approve(ctx context.Context, id string) (*Plan, error) {
	plan, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if len(plan.Loads) == 0 {
		return nil, fmt.Errorf("plan has no loads assigned; cannot write back")
	}

	// One delivery_route per load. Each push is idempotent upstream on
	// (vehicle_id, scheduled_date).
	for _, load := range plan.Loads {
		route := gable.DeliveryRoute{
			VehicleID:     load.VehicleID,
			DriverID:      load.DriverID,
			ScheduledDate: plan.PlanDate,
		}
		for _, st := range load.Stops {
			route.Stops = append(route.Stops, gable.RouteStop{
				OrderID:  st.OrderID,
				Sequence: st.Sequence,
				Lat:      st.Lat,
				Lng:      st.Lng,
			})
		}
		if err := s.sink.PushDeliveryRoute(ctx, route); err != nil {
			return nil, fmt.Errorf("write back to GableLBM (vehicle %s): %w", load.VehicleID, err)
		}
	}

	if err := s.repo.UpdateStatus(ctx, id, "APPROVED"); err != nil {
		return nil, err
	}
	plan.Status = "APPROVED"
	return plan, nil
}
