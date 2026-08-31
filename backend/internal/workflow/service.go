// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/catalog"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/depot"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/fleet"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/load"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/routing"
)

// Depot origin sources, recorded on the plan so the UI (and support) can see
// where a run's routing origin came from.
//
// The values live in internal/depot, which owns the one ladder this module and
// internal/routing both resolve through. They are re-exported here because they
// are part of this module's published plan payload (Plan.DepotSource) and are
// referenced by name across the codebase.
const (
	DepotSourceRequest  = depot.SourceRequest  // supplied on the ingest request
	DepotSourceBranch   = depot.SourceBranch   // the GableLBM yard every order on this run ships from
	DepotSourceConfig   = depot.SourceConfig   // this install's DEPOT_LAT/DEPOT_LNG
	DepotSourceCentroid = depot.SourceCentroid // centroid of the day's routable stops
	DepotSourceNone     = depot.SourceNone     // nothing to root on (no geocoded orders)
)

// defaultDeckHeightIn approximates deck height above road for clearance checks:
// total vehicle height = deck + tallest placement.
const defaultDeckHeightIn = 58.0

// ErrInvalidRequest marks a caller mistake — a missing or malformed field on the
// request itself — as distinct from a failure reaching or reading GableLBM.
// Handlers map it to 400; everything else on those paths is an upstream fault
// and maps to 502. Without the distinction an empty POST body reported
// "GableLBM is down", sending the operator to check an ERP that was fine.
var ErrInvalidRequest = errors.New("invalid request")

// Refusal is a workflow transition this module DECLINED on its own rules — a
// push gate, a missing prerequisite step, a request that names a truck this
// plan does not have. It is not a failure to reach GableLBM or the database.
//
// The distinction exists so the message can be shown. Every refusal in this
// package is a sentence written for a dispatcher, and the dispatch gate is only
// worth having if the person it stops is told what stopped them: "load capacity
// not cleared on: Truck 4 - Boom (GVW FAIL; 1 SKU(s) did not fit and were
// dropped: STONE-STEP-72 ×3 (truck full)) — re-pack or rebalance before
// pushing" is an instruction, and "Unprocessable Entity" is a shrug.
//
// Only a Refusal is forwarded verbatim to a client (see Handler.respondStep).
// An ordinary error is not, and must not be: `fetch vehicles: gable GET
// /api/integration/vehicles: status 500: <512 bytes of somebody else's
// response body>` is a diagnostic for a log, not a sentence for a yard.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

// refusedf builds a Refusal. Use it for anything a dispatcher should read;
// keep fmt.Errorf (and %w) for anything that wraps an upstream or storage fault.
func refusedf(format string, a ...any) error {
	return &Refusal{Msg: fmt.Sprintf(format, a...)}
}

// planStore is the persistence seam for workflow plans (satisfied by
// *Repository). It is declared consumer-side like every other seam in this
// module so the orchestrator can be exercised against an in-memory store with
// no Postgres. Update carries optimistic concurrency: it must reject a write
// whose plan.Version no longer matches the stored row (ErrVersionConflict).
type planStore interface {
	Create(ctx context.Context, p *Plan) error
	Update(ctx context.Context, p *Plan) error
	Get(ctx context.Context, id string) (*Plan, error)
	GetLatestForDate(ctx context.Context, date string) (*Plan, error)
}

// gableSource is the GableLBM integration surface the workflow consumes
// (satisfied by *gable.Client).
type gableSource interface {
	ListOrdersForDate(ctx context.Context, date string) ([]gable.Order, error)
	ListVehicles(ctx context.Context) ([]gable.Vehicle, error)
	ListLocations(ctx context.Context) ([]gable.Location, error)
	ListDrivers(ctx context.Context) ([]gable.Driver, error)
	PushDeliveryRoute(ctx context.Context, route gable.DeliveryRoute) error
}

// catalogSource resolves products to effective geometry (satisfied by *catalog.Service).
type catalogSource interface {
	ListEffectiveProducts(ctx context.Context) ([]catalog.EffectiveProduct, error)
}

// fleetProfiles supplies and auto-provisions vehicle profiles (satisfied by *fleet.Service).
type fleetProfiles interface {
	GetProfile(ctx context.Context, gableVehicleID string) (*fleet.Profile, error)
	UpsertProfile(ctx context.Context, gableVehicleID string, in fleet.ProfileInput) (*fleet.Profile, error)
}

// routeChecker runs restricted-point checks (satisfied by *compliance.Service).
type routeChecker interface {
	CheckRoute(ctx context.Context, req compliance.RouteCheckRequest) (*compliance.RouteCheckResult, error)
}

// aiBriefer generates the natural-language dispatch briefing (satisfied by
// *ai.Client). It is optional: when unconfigured the briefing endpoint reports
// "unavailable" and the core workflow is unaffected.
type aiBriefer interface {
	Configured() bool
	Model() string
	Generate(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (string, error)
}

// Config carries the workflow's tunable policy inputs (securement jurisdiction +
// anchor pitch for T1-5/T2-7, scheduled lock windows for T2-3, and this
// install's depot). Zero values fall back to sensible defaults so the service
// runs unconfigured.
type Config struct {
	SecurementJurisdiction    string
	SecurementAnchorSpacingIn float64
	LockMorningAt             string
	LockAfternoonAt           string

	// DepotLat/DepotLng are this dealer's fallback yard (DEPOT_LAT/DEPOT_LNG).
	// They are deployment configuration, not a code constant: nil means "not
	// configured", and a plan then roots at the centroid of its own stops
	// rather than at somebody else's yard. They are the whole-install answer
	// and are outranked by the branch the day's orders actually ship from,
	// which GableLBM knows per order.
	DepotLat *float64
	DepotLng *float64
}

// Service orchestrates the five-step dispatch workflow.
type Service struct {
	repo    planStore
	gable   gableSource
	catalog catalogSource
	fleet   fleetProfiles
	checker routeChecker
	ai      aiBriefer
	cfg     Config
}

func NewService(repo planStore, g gableSource, c catalogSource, f fleetProfiles, rc routeChecker, briefer aiBriefer, cfg Config) *Service {
	return &Service{repo: repo, gable: g, catalog: c, fleet: f, checker: rc, ai: briefer, cfg: cfg}
}

// Get returns a plan by id, with any scheduled lock evaluated for display.
func (s *Service) Get(ctx context.Context, id string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	applyLockSchedule(p, time.Now())
	return p, nil
}

// GetLatestForDate returns the most recent plan for a date.
func (s *Service) GetLatestForDate(ctx context.Context, date string) (*Plan, error) {
	p, err := s.repo.GetLatestForDate(ctx, date)
	if err != nil {
		return nil, err
	}
	applyLockSchedule(p, time.Now())
	return p, nil
}

// --- Step 1+2: ingest + deep analysis ---------------------------------------

// Ingest pulls every confirmed order scheduled for the date and analyzes each
// one: per-line effective geometry/weight, totals, shape profile, issues.
func (s *Service) Ingest(ctx context.Context, req IngestRequest) (*Plan, error) {
	if req.Date == "" {
		return nil, fmt.Errorf("%w: date is required", ErrInvalidRequest)
	}
	if _, err := time.Parse("2006-01-02", req.Date); err != nil {
		return nil, fmt.Errorf("%w: invalid date %q; expected YYYY-MM-DD", ErrInvalidRequest, req.Date)
	}

	orders, err := s.gable.ListOrdersForDate(ctx, req.Date)
	if err != nil {
		return nil, fmt.Errorf("fetch orders: %w", err)
	}

	products, err := s.catalog.ListEffectiveProducts(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog: %w", err)
	}
	byProduct := make(map[string]catalog.EffectiveProduct, len(products))
	for _, p := range products {
		byProduct[p.GableProductID] = p
	}

	analyses := make([]OrderAnalysis, 0, len(orders))
	for _, o := range orders {
		analyses = append(analyses, analyzeOrder(o, byProduct))
	}

	// Branch lookup is best effort and deliberately skipped when the request
	// already named a depot (nothing could outrank it anyway). A GableLBM that
	// predates /api/integration/locations answers 404 here; that must degrade
	// to the previous behaviour, not fail every plan for the day.
	var branches []gable.Location
	var branchesErr error
	if req.DepotLat == nil || req.DepotLng == nil {
		if branches, branchesErr = s.gable.ListLocations(ctx); branchesErr != nil {
			slog.Warn("could not list GableLBM branches; falling back down the depot chain",
				"date", req.Date, "err", branchesErr)
			branches = nil
		}
	}

	depotLat, depotLng, depotSource, depotNote := resolveDepot(req, s.cfg, analyses, branches)
	if branchesErr != nil && anyBranchID(analyses) {
		// The orders DID name a yard; we simply could not look it up. Say that,
		// rather than letting the note claim the branch was unknown.
		depotNote = fmt.Sprintf("could not read GableLBM's branches (%v); %s", branchesErr, depot.FallbackPhrase(depotSource))
	}
	if depotSource == DepotSourceNone {
		slog.Warn("no depot for workflow plan: DEPOT_LAT/DEPOT_LNG are unset and no order on this date has a geolocation to take a centroid from",
			"date", req.Date, "orders", len(analyses))
	}
	if depotNote != "" {
		slog.Info("workflow plan did not root at an order's branch",
			"date", req.Date, "depot_source", depotSource, "reason", depotNote)
	}

	plan := &Plan{
		PlanDate:         req.Date,
		Status:           StatusAnalyzed,
		DepotLat:         depotLat,
		DepotLng:         depotLng,
		DepotSource:      depotSource,
		DepotNote:        depotNote,
		Orders:           analyses,
		Loads:            []TruckLoad{},
		UnassignedOrders: []Stop{},
	}
	if err := s.repo.Create(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// resolveDepot picks a run's routing origin. The ladder itself —
// REQUEST -> BRANCH -> CONFIG -> CENTROID -> NONE, and every sentence it writes
// when the branch step declines — lives in internal/depot and is shared with
// internal/routing. This function's only job is to translate the workflow's
// vocabulary into the ladder's: which yards the day's orders ship from, and
// which of those orders can actually be driven to.
func resolveDepot(req IngestRequest, cfg Config, analyses []OrderAnalysis, branches []gable.Location) (lat, lng float64, source, note string) {
	return depot.Resolve(depot.Input{
		RequestLat: req.DepotLat,
		RequestLng: req.DepotLng,
		BranchIDs:  branchIDs(analyses),
		Branches:   branches,
		ConfigLat:  cfg.DepotLat,
		ConfigLng:  cfg.DepotLng,
		Stops:      routableStops(analyses),
	})
}

// branchIDs lists the yards this run's orders ship from, in the order they
// first appear. Orders with no branch id contribute nothing, which is how a
// GableLBM that predates orders.branch_id keeps its old, silent behaviour.
func branchIDs(analyses []OrderAnalysis) []string {
	ids := make([]string, 0, len(analyses))
	for _, a := range analyses {
		if a.BranchID != "" {
			ids = append(ids, a.BranchID)
		}
	}
	return ids
}

// routableStops is the centroid's input: only orders that can actually be
// driven to. An ungeocoded order must not drag the origin.
func routableStops(analyses []OrderAnalysis) []depot.Point {
	pts := make([]depot.Point, 0, len(analyses))
	for _, a := range analyses {
		if !a.Routable {
			continue
		}
		pts = append(pts, depot.Point{Lat: *a.Lat, Lng: *a.Lng})
	}
	return pts
}

// anyBranchID reports whether at least one ingested order named a yard, which
// is what makes a failed branch lookup worth telling the operator about.
func anyBranchID(analyses []OrderAnalysis) bool {
	for _, a := range analyses {
		if a.BranchID != "" {
			return true
		}
	}
	return false
}

// analyzeOrder resolves one order's lines against the effective catalog and
// derives weight/volume/shape metrics.
func analyzeOrder(o gable.Order, byProduct map[string]catalog.EffectiveProduct) OrderAnalysis {
	a := OrderAnalysis{
		OrderID:      o.ID,
		BranchID:     o.BranchID,
		CustomerName: o.CustomerName,
		Address:      o.Address,
		Lat:          o.Latitude,
		Lng:          o.Longitude,
		Lines:        []AnalyzedLine{},
		Issues:       []string{},
		Routable:     o.Latitude != nil && o.Longitude != nil,
	}

	for _, l := range o.Lines {
		line := AnalyzedLine{
			ProductID:     l.ProductID,
			SKU:           l.SKU,
			Quantity:      l.Quantity,
			UnitWeightLbs: l.WeightLbs,
		}
		if ep, ok := byProduct[l.ProductID]; ok {
			line.Name = ep.Name
			line.UnitLengthIn = ep.LengthIn
			line.UnitWidthIn = ep.WidthIn
			line.UnitHeightIn = ep.HeightIn
			line.Stackable = ep.Stackable
			line.HasGeometry = ep.HasGeometry
			if ep.WeightLbs > 0 {
				line.UnitWeightLbs = ep.WeightLbs
			}
		}
		a.Lines = append(a.Lines, line)
	}
	a.recomputeTotals()
	return a
}

// defaultDimTolerancePct grows an "average" variable-dimension override to a
// planning upper bound when the dispatcher does not supply an explicit tolerance.
const defaultDimTolerancePct = 15.0

// recomputeTotals re-derives every per-line and per-order metric (weight,
// volume, max length, piece count), the shape profile, and the issue list from
// the current line geometry. Shared by ingest analysis and the dimension-
// override path so both stay consistent.
func (a *OrderAnalysis) recomputeTotals() {
	a.TotalWeightLbs = 0
	a.TotalVolumeCuFt = 0
	a.MaxLengthIn = 0
	a.PieceCount = 0
	missingGeometry := 0
	for i := range a.Lines {
		l := &a.Lines[i]
		l.LineWeightLbs = round2(l.UnitWeightLbs * l.Quantity)
		l.LineVolumeCuFt = round2(l.UnitLengthIn * l.UnitWidthIn * l.UnitHeightIn / 1728.0 * l.Quantity)
		a.TotalWeightLbs += l.LineWeightLbs
		a.TotalVolumeCuFt += l.LineVolumeCuFt
		a.PieceCount += int(math.Round(l.Quantity))
		if l.UnitLengthIn > a.MaxLengthIn {
			a.MaxLengthIn = l.UnitLengthIn
		}
		if !l.HasGeometry {
			missingGeometry++
		}
	}
	a.TotalWeightLbs = round2(a.TotalWeightLbs)
	a.TotalVolumeCuFt = round2(a.TotalVolumeCuFt)

	switch {
	case a.MaxLengthIn >= 192:
		a.ShapeProfile = ShapeLongLoad
	case a.MaxLengthIn > 0 && a.MaxLengthIn <= 96:
		a.ShapeProfile = ShapeCompact
	default:
		a.ShapeProfile = ShapeMixed
	}

	a.Issues = []string{}
	if !a.Routable {
		a.Issues = append(a.Issues, "no delivery geolocation — cannot route")
	}
	if missingGeometry > 0 {
		a.Issues = append(a.Issues, fmt.Sprintf("%d line(s) missing digital-twin geometry", missingGeometry))
	}
}

// --- Step 3: assign orders to trucks + sequence routes -----------------------

// Assign splits the analyzed orders across the live fleet (CVRP by weight +
// volume) and sequences each truck's route from the depot. On a locked run it
// refuses to reshuffle unless override (manual approval) is supplied (T2-3).
func (s *Service) Assign(ctx context.Context, id string, override bool, approvedBy string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := gateReshuffle(p, override, approvedBy, "re-assigning trucks"); err != nil {
		return nil, err
	}

	byOrder := orderIndex(p)
	var rstops []routing.Stop
	for _, a := range p.Orders {
		if !a.Routable {
			continue
		}
		rstops = append(rstops, routing.Stop{
			OrderID:    a.OrderID,
			Lat:        *a.Lat,
			Lng:        *a.Lng,
			Address:    a.Address,
			WeightLbs:  a.TotalWeightLbs,
			VolumeCuFt: a.TotalVolumeCuFt,
		})
	}

	vehicles, err := s.gable.ListVehicles(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch vehicles: %w", err)
	}
	drivers, err := s.gable.ListDrivers(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch drivers: %w", err)
	}

	// Usable bed volume per vehicle (T2-2) — a stored fleet profile when one
	// exists, else the type-based default. Lets the assignment cap a truck by
	// space as well as weight without provisioning a profile for every vehicle.
	volCapByVehicle := make(map[string]float64, len(vehicles))
	for _, v := range vehicles {
		volCapByVehicle[v.ID] = s.usableBedVolume(ctx, v)
	}

	rloads, unassigned := sweepAssign(vehicles, rstops, p.DepotLat, p.DepotLng, volCapByVehicle)
	routing.AssignDrivers(drivers, rloads)

	priSet := prioritySet(p)
	p.Loads = make([]TruckLoad, 0, len(rloads))
	for _, rl := range rloads {
		ordered, dist, dur := sequenceWithPriority(p.DepotLat, p.DepotLng, rl.Stops, priSet)
		tl := TruckLoad{
			VehicleID:         rl.VehicleID,
			VehicleName:       rl.VehicleName,
			DriverID:          rl.DriverID,
			DriverName:        rl.DriverName,
			CapacityWeightLbs: rl.CapacityWeightLbs,
			TotalWeightLbs:    round2(rl.TotalWeightLbs),
			TotalDistanceMi:   dist,
			TotalDurationMin:  dur,
			Stops:             make([]Stop, 0, len(ordered)),
		}
		for _, st := range ordered {
			tl.Stops = append(tl.Stops, toWorkflowStop(st, byOrder))
		}
		p.Loads = append(p.Loads, tl)
	}

	p.UnassignedOrders = make([]Stop, 0, len(unassigned))
	for _, st := range unassigned {
		p.UnassignedOrders = append(p.UnassignedOrders, toWorkflowStop(st, byOrder))
	}

	p.Status = StatusAssigned
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// --- Step 4: pack every truck (LIFO bundles) ---------------------------------

// Pack 3D-packs every assigned truck: stops load in reverse route order so the
// first delivery is the last material on (rear of bed, first off).
func (s *Service) Pack(ctx context.Context, id string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(p.Loads) == 0 {
		return nil, refusedf("no truck assignments yet — run assign first")
	}

	vehicles, err := s.gable.ListVehicles(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch vehicles: %w", err)
	}
	vehiclesByID := make(map[string]gable.Vehicle, len(vehicles))
	for _, v := range vehicles {
		vehiclesByID[v.ID] = v
	}

	for i := range p.Loads {
		if err := s.packLoad(ctx, p, &p.Loads[i], vehiclesByID, 0); err != nil {
			return nil, err
		}
	}

	p.Status = StatusPacked
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// packLoad solves one truck's 3D placement. maxHeightIn > 0 caps the load
// height below the bed envelope (used by compliance load adjustment under a
// low-clearance route).
func (s *Service) packLoad(ctx context.Context, p *Plan, l *TruckLoad, vehiclesByID map[string]gable.Vehicle, maxHeightIn float64) error {
	profile, err := s.ensureProfile(ctx, l, vehiclesByID)
	if err != nil {
		return err
	}
	l.Bed = &BedDims{LengthIn: profile.BedLengthIn, WidthIn: profile.BedWidthIn, HeightIn: profile.BedHeightIn}

	byOrder := orderIndex(p)
	stops := make([]load.StopItems, 0, len(l.Stops))
	for _, st := range l.Stops {
		a, ok := byOrder[st.OrderID]
		if !ok {
			continue
		}
		si := load.StopItems{OrderID: st.OrderID, StopSequence: st.Sequence}
		for _, line := range a.Lines {
			si.Items = append(si.Items, load.Item{
				ProductID: line.ProductID,
				SKU:       line.SKU,
				Quantity:  int(math.Round(line.Quantity)),
				LengthIn:  line.UnitLengthIn,
				WidthIn:   line.UnitWidthIn,
				HeightIn:  line.UnitHeightIn,
				WeightLbs: line.UnitWeightLbs,
				Stackable: line.Stackable,
			})
		}
		stops = append(stops, si)
	}

	v := toSolverVehicle(profile)
	v.SecurementJurisdiction = s.cfg.SecurementJurisdiction
	v.AnchorSpacingIn = s.cfg.SecurementAnchorSpacingIn
	if maxHeightIn > 0 && maxHeightIn < v.BedHeightIn {
		v.BedHeightIn = maxHeightIn
	}
	lp := load.SolveSequencedBundles(v, stops)
	prev := l.LoadPlan
	l.LoadPlan = &lp
	l.Compliance = nil // packing changed — any previous review is stale
	invalidateSignOff(l, prev, &lp)
	return nil
}

// invalidateSignOff clears the yard sign-off when a re-pack produced a
// DIFFERENT physical load from the one that was signed for.
//
// The proof-of-load gate exists so no truck leaves the yard without a photo of
// how it was actually loaded and a human attesting to it. AttachProof already
// encodes half of that rule — new evidence supersedes a prior sign-off — but
// the other half was missing: the packing itself can change AFTER a sign-off,
// through Pack, through Resequence, through the compliance reviewer's
// height-capped LOAD_ADJUST re-pack, and through a cross-truck weight
// rebalance. None of those touched Proof, so a sign-off taken against one
// arrangement of the deck still released a truck packed a different way — with
// different pack steps, a different securement plan, and possibly cargo that no
// longer fits at all. The signature said "I saw this load"; the manifest was
// somebody else's.
//
// The attachments are deliberately KEPT: they are photographs of a truck, and
// deleting a dispatcher's evidence is not this function's business. Only the
// attestation is withdrawn, so the yard re-checks the deck and signs again —
// which is the same thing AttachProof does when a new photo lands.
//
// A re-pack that lands on the identical plan (re-running Pack on an unchanged
// order, the common accidental double-click) is a no-op, so a dispatcher is not
// forced to chase a fresh signature for nothing.
func invalidateSignOff(l *TruckLoad, prev, next *load.Plan) {
	if l.Proof == nil || !l.Proof.SignedOff || samePacking(prev, next) {
		return
	}
	l.Proof.SignedOff = false
	l.Proof.SignedAt = nil
	l.Proof.Note = strings.TrimSpace(l.Proof.Note + " (sign-off withdrawn: this truck was re-packed after it was signed for)")
}

// samePacking reports whether two solves describe the same physical load: the
// same units in the same places in the same order, the same cargo left behind,
// and the same totals. Fail-closed — a nil previous plan is not the same as
// anything, so the first solve after a sign-off always invalidates it.
func samePacking(a, b *load.Plan) bool {
	if a == nil || b == nil {
		return false
	}
	return a.TotalWeightLbs == b.TotalWeightLbs &&
		a.UnmodeledWeightLbs == b.UnmodeledWeightLbs &&
		a.MaxLoadHeightIn == b.MaxLoadHeightIn &&
		reflect.DeepEqual(a.Placements, b.Placements) &&
		reflect.DeepEqual(a.Unplaced, b.Unplaced) &&
		reflect.DeepEqual(a.Securement, b.Securement)
}

// Resequence manually reorders one truck's stops (the dispatcher's packing-
// stage adjustment), then re-packs that truck and recomputes its route totals.
// On a locked run it requires override (manual approval) (T2-3).
func (s *Service) Resequence(ctx context.Context, id, vehicleID string, orderIDs []string, override bool, approvedBy string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := gateReshuffle(p, override, approvedBy, "re-sequencing a route"); err != nil {
		return nil, err
	}

	var l *TruckLoad
	for i := range p.Loads {
		if p.Loads[i].VehicleID == vehicleID {
			l = &p.Loads[i]
			break
		}
	}
	if l == nil {
		return nil, refusedf("no load for vehicle %s in this plan", vehicleID)
	}

	byOrder := make(map[string]Stop, len(l.Stops))
	for _, st := range l.Stops {
		byOrder[st.OrderID] = st
	}
	if len(orderIDs) != len(l.Stops) {
		return nil, refusedf("order_ids must be a permutation of the load's %d stops", len(l.Stops))
	}
	reordered := make([]Stop, 0, len(orderIDs))
	for i, oid := range orderIDs {
		st, ok := byOrder[oid]
		if !ok {
			return nil, refusedf("order %s is not on this load", oid)
		}
		st.Sequence = i + 1
		reordered = append(reordered, st)
		delete(byOrder, oid)
	}
	l.Stops = reordered
	l.TotalDistanceMi, l.TotalDurationMin = routeTotals(p.DepotLat, p.DepotLng, l.Stops)

	vehicles, err := s.gable.ListVehicles(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch vehicles: %w", err)
	}
	vehiclesByID := make(map[string]gable.Vehicle, len(vehicles))
	for _, v := range vehicles {
		vehiclesByID[v.ID] = v
	}
	if err := s.packLoad(ctx, p, l, vehiclesByID, 0); err != nil {
		return nil, err
	}

	// A manual resequence invalidates any later-stage artifacts.
	if p.Status == StatusReviewed || p.Status == StatusPushed {
		p.Status = StatusPacked
	}
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// --- Step 6: push to the GableLBM dispatch board ------------------------------

// capacityStatusClears reports whether a load-solver status (GVWStatus or an
// AxleLoad.Status) is safe to dispatch on. It is deliberately a whitelist:
// PASS is clear and WARN is "loaded near the rating but still within it".
// EVERYTHING else blocks — FAIL, an UNKNOWN/unrated status, an empty string
// (the solver skips the GVW check entirely when the profile has no GVWR, which
// would otherwise read as a confident PASS), or any status a future solver
// adds. Fail-closed is the only correct default for a module sold on GVW/axle
// enforcement.
func capacityStatusClears(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "PASS", "WARN":
		return true
	default:
		return false
	}
}

// axleVerdictIsAdvisory reports whether an over-rating axle verdict is this
// model's ESTIMATE rather than a measurement — the case that must warn instead
// of refuse.
//
// It reads the solver's own confidence flag (load.AxleLoad.Advisory, which the
// load package doc explains in "HOW CALLERS MUST GATE ON THIS"). Three things
// must all hold, and the zero value fails all three, so anything that does not
// positively assert "this is an estimate over a known rating" keeps blocking:
//
//   - Advisory — the split came from the unvalidated bed-origin datum with no
//     overhang lever. ROADMAP §3: advisory "until validated against certified
//     scale tickets". A future calibrated source sets Advisory=false and this
//     gate blocks on it again, with no further change here.
//   - the axle is RATED — a zero rating is StatusUnknown, a fleet-profile defect
//     rather than an estimate, and there is nothing to be advisory about.
//   - the verdict is FAIL — the only status that is both non-clearing and
//     actually computed. UNKNOWN, "", and any status a future solver adds are
//     not estimates; they stay blocking.
func axleVerdictIsAdvisory(a load.AxleLoad) bool {
	return a.Advisory && a.MaxWeightLbs > 0 &&
		strings.EqualFold(strings.TrimSpace(a.Status), load.StatusFail)
}

// capacityFindings splits one truck's own load solve into what BLOCKS a push
// and what the dispatcher must merely SEE.
//
// blocking is the exact, measured trouble: an over-GVWR (or unjudgeable) gross
// weight, an axle whose rating the profile never supplied, or cargo the packer
// could not physically fit — which would ship the customer short with nothing on
// the manifest to show it. Empty ⇒ the truck's own verdict permits departure.
//
// advisories are real signals with soft numbers behind them. They never refuse a
// departure, and callers MUST render them: an advisory nobody sees is worse than
// no advisory at all.
//
//   - a per-axle over-rating computed on the model's documented-wrong datum
//     (see axleVerdictIsAdvisory);
//   - articles with no recorded geometry: they are not "dropped", they ride —
//     they simply are not in the 3D plan, so the yard loads them by hand and
//     their weight is in the gross but not in the per-axle split.
func capacityFindings(l TruckLoad) (blocking, advisories []string) {
	if l.LoadPlan == nil {
		return []string{"not packed"}, nil
	}
	if !capacityStatusClears(l.LoadPlan.GVWStatus) {
		blocking = append(blocking, fmt.Sprintf("GVW %s", statusLabel(l.LoadPlan.GVWStatus)))
	}
	for _, a := range l.LoadPlan.AxleLoads {
		if capacityStatusClears(a.Status) {
			continue
		}
		if axleVerdictIsAdvisory(a) {
			advisories = append(advisories, fmt.Sprintf(
				"axle %d is over its rating on the ADVISORY split (%s of %s lb, %.0f%%) — the per-axle model uses the bed origin as the steer datum and does not model overhang, so verify at a certified scale before dispatch",
				a.AxleNumber, formatLbs(a.WeightLbs), formatLbs(a.MaxWeightLbs), a.Utilization*100))
			continue
		}
		blocking = append(blocking, fmt.Sprintf("axle %d %s", a.AxleNumber, statusLabel(a.Status)))
	}

	dropped, noGeometry := load.SplitUnplaced(l.LoadPlan.Unplaced)
	if n := len(dropped); n > 0 {
		blocking = append(blocking, fmt.Sprintf("%d SKU(s) did not fit and were dropped: %s",
			n, strings.Join(load.UnplacedStrings(dropped), ", ")))
	}
	if n := len(noGeometry); n > 0 {
		msg := fmt.Sprintf("%d line(s) have no digital-twin geometry and are not in the 3D plan — load by hand and check the manifest: %s",
			n, strings.Join(load.UnplacedStrings(noGeometry), ", "))
		if w := l.LoadPlan.UnmodeledWeightLbs; w > 0 {
			msg += fmt.Sprintf(" (%s lb in the gross weight, absent from the per-axle split)", formatLbs(w))
		}
		advisories = append(advisories, msg)
	}
	return blocking, advisories
}

// blockingCapacityReasons lists only the findings that refuse a departure.
func blockingCapacityReasons(l TruckLoad) []string {
	blocking, _ := capacityFindings(l)
	return blocking
}

// formatLbs renders a weight with thousands separators for operator messages.
func formatLbs(lbs int64) string {
	s := strconv.FormatInt(lbs, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, d := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// statusLabel renders an empty/unset status readably in an operator message.
func statusLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return "UNKNOWN (not evaluated)"
	}
	return strings.ToUpper(strings.TrimSpace(s))
}

// Push writes every truck's route + packing manifest to GableLBM. It is the
// last gate before a load is live on the dispatch board and the yard's
// Pack-Trucks surface, so it refuses on any of:
//   - a restricted-point compliance FAIL (bridge weight / overpass clearance);
//   - the truck's OWN measured capacity verdict — an over-GVWR gross weight, or
//     an axle whose rating the fleet profile never supplied — regardless of what
//     the route crosses;
//   - cargo the packer could not FIT (a blocking load.Unplaced entry), which
//     would ship the customer short with nothing on the manifest to show it;
//   - a missing yard proof-of-load or sign-off (T1-6).
//
// It deliberately does NOT refuse on the advisory findings (see
// capacityFindings): an over-rating on the per-axle estimate, or a line with no
// digital-twin geometry. Both are carried onto the review, logged here, and
// written onto the manifest that reaches the yard — the dispatcher decides.
func (s *Service) Push(ctx context.Context, id string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(p.Loads) == 0 {
		return nil, refusedf("nothing to push — no truck loads")
	}

	var failing []string
	var overCapacity []string
	var unsigned []string
	for _, l := range p.Loads {
		if l.LoadPlan == nil {
			return nil, refusedf("truck %s is not packed yet", l.VehicleName)
		}
		if l.Compliance == nil {
			return nil, refusedf("truck %s has not passed route review yet", l.VehicleName)
		}
		if l.Compliance.Status == "FAIL" {
			failing = append(failing, l.VehicleName)
		}
		// The truck's own GVW rating and any cargo that did not fit — a route
		// with no weight-restricted bridge must not launder an overweight or
		// short-loaded truck onto the dispatch board.
		blocking, advisories := capacityFindings(l)
		if len(blocking) > 0 {
			overCapacity = append(overCapacity, fmt.Sprintf("%s (%s)", l.VehicleName, strings.Join(blocking, "; ")))
		}
		// Advisory findings do not refuse. Log them so a dispatched load that
		// later scales over is traceable to the warning nobody acted on; they
		// also ride on the review and on the manifest the yard reads.
		for _, a := range advisories {
			slog.Warn("dispatching a load with an unresolved capacity advisory",
				"plan", p.ID, "date", p.PlanDate, "vehicle", l.VehicleName, "advisory", a)
		}
		// Yard proof-of-load + sign-off gate (T1-6): no truck leaves the yard
		// without photo/video proof and a sign-off.
		if !l.Proof.Ready() {
			unsigned = append(unsigned, l.VehicleName)
		}
	}
	if len(failing) > 0 {
		return nil, refusedf("compliance FAIL on: %s — resolve before pushing", strings.Join(failing, ", "))
	}
	if len(overCapacity) > 0 {
		return nil, refusedf("load capacity not cleared on: %s — re-pack or rebalance before pushing", strings.Join(overCapacity, ", "))
	}
	if len(unsigned) > 0 {
		return nil, refusedf("yard proof + sign-off required before depart on: %s", strings.Join(unsigned, ", "))
	}

	for _, l := range p.Loads {
		route := gable.DeliveryRoute{
			VehicleID:     l.VehicleID,
			DriverID:      l.DriverID,
			ScheduledDate: p.PlanDate,
			LoadManifest:  buildManifest(p, l),
		}
		for _, st := range l.Stops {
			route.Stops = append(route.Stops, gable.RouteStop{
				OrderID:  st.OrderID,
				Sequence: st.Sequence,
				Lat:      st.Lat,
				Lng:      st.Lng,
			})
		}
		if err := s.gable.PushDeliveryRoute(ctx, route); err != nil {
			return nil, fmt.Errorf("write back to GableLBM (truck %s): %w", l.VehicleName, err)
		}
	}

	p.Status = StatusPushed
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// buildManifest assembles the yard-facing packing manifest for one truck. It is
// stored verbatim on the GableLBM delivery route and rendered by the yard
// "Pack Trucks" surface.
func buildManifest(p *Plan, l TruckLoad) map[string]any {
	byOrder := orderIndex(p)
	stops := make([]map[string]any, 0, len(l.Stops))
	for _, st := range l.Stops {
		pieceCount := 0
		if a, ok := byOrder[st.OrderID]; ok {
			pieceCount = a.PieceCount
		}
		stops = append(stops, map[string]any{
			"order_id":      st.OrderID,
			"sequence":      st.Sequence,
			"customer_name": st.CustomerName,
			"address":       st.Address,
			"weight_lbs":    st.WeightLbs,
			"piece_count":   pieceCount,
		})
	}
	skuNames := map[string]string{}
	for _, a := range p.Orders {
		for _, line := range a.Lines {
			if line.Name != "" {
				skuNames[line.SKU] = line.Name
			}
		}
	}
	// The two kinds of unplaced article are separated here because the yard does
	// two DIFFERENT things with them, and a single list made the manifest lie
	// about both:
	//
	//   dropped        — did not fit. It is NOT on the truck; the customer ships
	//                    short and somebody has to be told.
	//   no_geometry    — has no recorded dimensions, so it is not in the 3D
	//                    packing steps. It IS on the truck: the yard loads it by
	//                    hand, and it is in total_weight_lbs but not in
	//                    axle_loads (unmodeled_weight_lbs says how much).
	//
	// Every key is always emitted (never omitted when empty) so "nothing was
	// dropped" is an explicit statement rather than a gap. `unplaced` keeps the
	// full typed list so a consumer can re-derive either set.
	unplaced := l.LoadPlan.Unplaced
	if unplaced == nil {
		unplaced = []load.Unplaced{}
	}
	dropped, noGeometry := load.SplitUnplaced(unplaced)
	blocking, advisories := capacityFindings(l)
	if advisories == nil {
		advisories = []string{}
	}
	return map[string]any{
		// version 2: `unplaced` carries typed reasons (was a flat string list),
		// and `dropped` / `no_geometry` / `advisories` / `unmodeled_weight_lbs`
		// were added alongside it.
		"version":              2,
		"plan_date":            p.PlanDate,
		"vehicle_id":           l.VehicleID,
		"vehicle_name":         l.VehicleName,
		"driver_name":          l.DriverName,
		"bed":                  l.Bed,
		"total_weight_lbs":     l.LoadPlan.TotalWeightLbs,
		"unmodeled_weight_lbs": l.LoadPlan.UnmodeledWeightLbs,
		"gvw_status":           l.LoadPlan.GVWStatus,
		"max_load_height_in":   l.LoadPlan.MaxLoadHeightIn,
		"axle_loads":           l.LoadPlan.AxleLoads,
		"axle_loads_advisory":  true,
		"unplaced":             unplaced,
		"dropped":              load.UnplacedStrings(dropped),
		"no_geometry":          load.UnplacedStrings(noGeometry),
		"advisories":           advisories,
		"capacity_cleared":     len(blocking) == 0,
		"stops":                stops,
		"steps":                l.LoadPlan.Placements, // already in pack order with Step set
		"sku_names":            skuNames,
		"securement":           l.LoadPlan.Securement,
		"compliance":           l.Compliance,
		"proof":                l.Proof,
	}
}

// --- helpers -----------------------------------------------------------------

func orderIndex(p *Plan) map[string]*OrderAnalysis {
	m := make(map[string]*OrderAnalysis, len(p.Orders))
	for i := range p.Orders {
		m[p.Orders[i].OrderID] = &p.Orders[i]
	}
	return m
}

func toWorkflowStop(st routing.Stop, byOrder map[string]*OrderAnalysis) Stop {
	out := Stop{
		OrderID:   st.OrderID,
		Sequence:  st.Sequence,
		Lat:       st.Lat,
		Lng:       st.Lng,
		Address:   st.Address,
		WeightLbs: round2(st.WeightLbs),
	}
	if a, ok := byOrder[st.OrderID]; ok {
		out.CustomerName = a.CustomerName
		out.Priority = a.Priority
	}
	return out
}

// prioritySet returns the set of order IDs marked deliver-first (T2-1).
func prioritySet(p *Plan) map[string]bool {
	m := make(map[string]bool)
	for _, a := range p.Orders {
		if a.Priority {
			m[a.OrderID] = true
		}
	}
	return m
}

// sequenceWithPriority pins priority stops to the front of the route, then
// optimizes the rest around them. With no priority stops it is exactly the
// normal depot-rooted optimization. Priority stops are themselves optimized
// (from the depot); the remaining stops are then optimized starting from the
// last priority stop so the hand-off leg is realistic. Sequence numbers are
// renumbered 1..n across the combined route and the totals are summed.
func sequenceWithPriority(depotLat, depotLng float64, rstops []routing.Stop, isPriority map[string]bool) ([]routing.Stop, float64, float64) {
	if len(rstops) == 0 {
		return []routing.Stop{}, 0, 0
	}
	var pri, rest []routing.Stop
	for _, s := range rstops {
		if isPriority[s.OrderID] {
			pri = append(pri, s)
		} else {
			rest = append(rest, s)
		}
	}
	if len(pri) == 0 {
		return routing.OptimizeSequence(depotLat, depotLng, rest)
	}

	seqPri, d1, t1 := routing.OptimizeSequence(depotLat, depotLng, pri)
	startLat, startLng := depotLat, depotLng
	if len(seqPri) > 0 {
		last := seqPri[len(seqPri)-1]
		startLat, startLng = last.Lat, last.Lng
	}
	seqRest, d2, t2 := routing.OptimizeSequence(startLat, startLng, rest)

	combined := make([]routing.Stop, 0, len(seqPri)+len(seqRest))
	combined = append(combined, seqPri...)
	combined = append(combined, seqRest...)
	for i := range combined {
		combined[i].Sequence = i + 1
	}
	return combined, round2(d1 + d2), round2(t1 + t2)
}

// SetPriority toggles an order's deliver-first flag (dealer override T2-1) and
// re-sequences (and re-packs) the truck carrying it, pinning priority stops to
// the front of the route. It is safe to call at any stage; later-stage
// artifacts (review/push) are invalidated so the dispatcher re-runs them.
func (s *Service) SetPriority(ctx context.Context, id, orderID string, priority bool, override bool, approvedBy string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := gateReshuffle(p, override, approvedBy, "changing delivery priority"); err != nil {
		return nil, err
	}

	found := false
	for i := range p.Orders {
		if p.Orders[i].OrderID == orderID {
			if priority && !p.Orders[i].Routable {
				return nil, refusedf("order %s has no geolocation and cannot be prioritized", orderID)
			}
			p.Orders[i].Priority = priority
			found = true
			break
		}
	}
	if !found {
		return nil, refusedf("order %s is not part of this plan", orderID)
	}

	// Reflect the flag onto any materialized stop (assigned or unassigned).
	for i := range p.Loads {
		for j := range p.Loads[i].Stops {
			if p.Loads[i].Stops[j].OrderID == orderID {
				p.Loads[i].Stops[j].Priority = priority
			}
		}
	}
	for i := range p.UnassignedOrders {
		if p.UnassignedOrders[i].OrderID == orderID {
			p.UnassignedOrders[i].Priority = priority
		}
	}

	// Re-sequence (and re-pack) the truck carrying this order.
	var target *TruckLoad
	for i := range p.Loads {
		for _, st := range p.Loads[i].Stops {
			if st.OrderID == orderID {
				target = &p.Loads[i]
				break
			}
		}
		if target != nil {
			break
		}
	}
	if target != nil {
		resequenceOptimal(p, target)
		if target.LoadPlan != nil {
			vehicles, err := s.gable.ListVehicles(ctx)
			if err != nil {
				return nil, fmt.Errorf("fetch vehicles: %w", err)
			}
			vehiclesByID := make(map[string]gable.Vehicle, len(vehicles))
			for _, v := range vehicles {
				vehiclesByID[v.ID] = v
			}
			if err := s.packLoad(ctx, p, target, vehiclesByID, 0); err != nil {
				return nil, err
			}
		}
		if p.Status == StatusReviewed || p.Status == StatusPushed {
			p.Status = StatusPacked
		}
	}

	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// SetLineDimensions applies a per-order dimension override for a variable-
// dimension SKU (T2-2). When only an average is known a tolerance grows the
// dims to a planning upper bound. The override feeds the digital twin + packing;
// the truck carrying the order is re-packed and later-stage artifacts cleared.
func (s *Service) SetLineDimensions(ctx context.Context, id, orderID string, req DimensionOverrideRequest) (*Plan, error) {
	if req.ProductID == "" && req.SKU == "" {
		return nil, refusedf("product_id or sku is required to target a line")
	}
	if req.LengthIn <= 0 || req.WidthIn <= 0 || req.HeightIn <= 0 {
		return nil, refusedf("length_in, width_in and height_in must be positive")
	}

	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	var order *OrderAnalysis
	for i := range p.Orders {
		if p.Orders[i].OrderID == orderID {
			order = &p.Orders[i]
			break
		}
	}
	if order == nil {
		return nil, refusedf("order %s is not part of this plan", orderID)
	}

	tol := req.TolerancePct
	if tol == 0 && strings.EqualFold(req.Source, "AVERAGE") {
		tol = defaultDimTolerancePct
	}
	f := 1 + tol/100

	matched := 0
	for i := range order.Lines {
		l := &order.Lines[i]
		if req.ProductID != "" {
			if l.ProductID != req.ProductID {
				continue
			}
		} else if !strings.EqualFold(l.SKU, req.SKU) {
			continue
		}
		l.UnitLengthIn = round2(req.LengthIn * f)
		l.UnitWidthIn = round2(req.WidthIn * f)
		l.UnitHeightIn = round2(req.HeightIn * f)
		l.HasGeometry = true
		l.DimOverride = &DimOverride{
			LengthIn:     req.LengthIn,
			WidthIn:      req.WidthIn,
			HeightIn:     req.HeightIn,
			TolerancePct: tol,
			Source:       req.Source,
			Note:         req.Note,
		}
		matched++
	}
	if matched == 0 {
		return nil, refusedf("no line in order %s matched the override target", orderID)
	}

	order.recomputeTotals()
	if err := s.repackOrderTruck(ctx, p, orderID); err != nil {
		return nil, err
	}
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// repackOrderTruck re-packs the truck carrying orderID (if assigned + packed)
// and invalidates any later-stage (review/push) artifacts so the dispatcher
// re-runs them. A no-op when the order is unassigned or not yet packed.
func (s *Service) repackOrderTruck(ctx context.Context, p *Plan, orderID string) error {
	var target *TruckLoad
	for i := range p.Loads {
		for _, st := range p.Loads[i].Stops {
			if st.OrderID == orderID {
				target = &p.Loads[i]
				break
			}
		}
		if target != nil {
			break
		}
	}
	if target == nil || target.LoadPlan == nil {
		return nil
	}

	vehicles, err := s.gable.ListVehicles(ctx)
	if err != nil {
		return fmt.Errorf("fetch vehicles: %w", err)
	}
	vehiclesByID := make(map[string]gable.Vehicle, len(vehicles))
	for _, v := range vehicles {
		vehiclesByID[v.ID] = v
	}
	if err := s.packLoad(ctx, p, target, vehiclesByID, 0); err != nil {
		return err
	}
	if p.Status == StatusReviewed || p.Status == StatusPushed {
		p.Status = StatusPacked
	}
	return nil
}

// routeTotals computes path distance/duration for a fixed stop order.
func routeTotals(depotLat, depotLng float64, stops []Stop) (float64, float64) {
	const avgSpeedMph = 35.0
	total := 0.0
	pLat, pLng := depotLat, depotLng
	for _, st := range stops {
		total += routing.HaversineMiles(pLat, pLng, st.Lat, st.Lng)
		pLat, pLng = st.Lat, st.Lng
	}
	return round2(total), round2(total / avgSpeedMph * 60.0)
}

// usableBedVolume returns a vehicle's usable bed volume (ft³) for the
// assignment volume cap: the stored fleet profile's bed when one exists, else
// the type-based default. Read-only — it never provisions a profile.
func (s *Service) usableBedVolume(ctx context.Context, v gable.Vehicle) float64 {
	if prof, err := s.fleet.GetProfile(ctx, v.ID); err == nil && prof != nil {
		return load.UsableBedVolumeCuFt(prof.BedLengthIn, prof.BedWidthIn, prof.BedHeightIn)
	}
	in := defaultProfileInput(v)
	return load.UsableBedVolumeCuFt(in.BedLengthIn, in.BedWidthIn, in.BedHeightIn)
}

// ensureProfile fetches the truck's fleet profile, auto-provisioning a
// sensible default from the GableLBM vehicle type when none exists yet (the
// dispatcher can refine it later on the Fleet page).
func (s *Service) ensureProfile(ctx context.Context, l *TruckLoad, vehiclesByID map[string]gable.Vehicle) (*fleet.Profile, error) {
	profile, err := s.fleet.GetProfile(ctx, l.VehicleID)
	if err == nil {
		return profile, nil
	}
	if err != fleet.ErrNotFound {
		return nil, fmt.Errorf("load fleet profile for %s: %w", l.VehicleName, err)
	}

	v, ok := vehiclesByID[l.VehicleID]
	if !ok {
		v = gable.Vehicle{ID: l.VehicleID, Name: l.VehicleName, VehicleType: "FLATBED"}
	}
	input := defaultProfileInput(v)
	created, err := s.fleet.UpsertProfile(ctx, l.VehicleID, input)
	if err != nil {
		return nil, fmt.Errorf("auto-provision fleet profile for %s: %w", l.VehicleName, err)
	}
	return created, nil
}

// defaultProfileInput derives a load-planning profile from the GableLBM
// vehicle record alone (type + payload capacity).
func defaultProfileInput(v gable.Vehicle) fleet.ProfileInput {
	type spec struct {
		bedL, bedW, bedH float64
		tare             int64
		steer, drive     int64
		drivePos         float64
	}
	sp := spec{bedL: 288, bedW: 96, bedH: 96, tare: 14000, steer: 12000, drive: 21000, drivePos: 240} // flatbed default
	t := strings.ToUpper(v.VehicleType)
	switch {
	case strings.Contains(t, "BOX"):
		sp = spec{bedL: 312, bedW: 100, bedH: 102, tare: 12500, steer: 10000, drive: 17500, drivePos: 260}
	case strings.Contains(t, "PICKUP"):
		sp = spec{bedL: 98, bedW: 64, bedH: 21, tare: 6500, steer: 4800, drive: 6500, drivePos: 160}
	case strings.Contains(t, "VAN"):
		sp = spec{bedL: 144, bedW: 70, bedH: 64, tare: 6000, steer: 4600, drive: 5500, drivePos: 140}
	case strings.Contains(t, "CRANE"):
		sp = spec{bedL: 264, bedW: 96, bedH: 96, tare: 22000, steer: 14000, drive: 23000, drivePos: 230}
	}

	gvwr := sp.tare + 12000
	if v.CapacityWeightLbs != nil && *v.CapacityWeightLbs > 0 {
		gvwr = sp.tare + int64(*v.CapacityWeightLbs)
	}
	name := v.Name
	if name == "" {
		name = v.ID
	}
	return fleet.ProfileInput{
		Name:          name,
		BedLengthIn:   sp.bedL,
		BedWidthIn:    sp.bedW,
		BedHeightIn:   sp.bedH,
		GVWRLbs:       gvwr,
		TareWeightLbs: sp.tare,
		Axles: []fleet.AxleInput{
			{AxleNumber: 1, MaxWeightLbs: sp.steer, PositionFromFrontIn: 0, AxleType: "STEER"},
			{AxleNumber: 2, MaxWeightLbs: sp.drive, PositionFromFrontIn: sp.drivePos, AxleType: "DRIVE"},
		},
	}
}

func toSolverVehicle(p *fleet.Profile) load.Vehicle {
	v := load.Vehicle{
		GableVehicleID: p.GableVehicleID,
		BedLengthIn:    p.BedLengthIn,
		BedWidthIn:     p.BedWidthIn,
		BedHeightIn:    p.BedHeightIn,
		GVWRLbs:        p.GVWRLbs,
		TareWeightLbs:  p.TareWeightLbs,
	}
	for _, a := range p.Axles {
		v.Axles = append(v.Axles, load.Axle{
			AxleNumber:          a.AxleNumber,
			MaxWeightLbs:        a.MaxWeightLbs,
			PositionFromFrontIn: a.PositionFromFrontIn,
			AxleType:            a.AxleType,
		})
	}
	return v
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
