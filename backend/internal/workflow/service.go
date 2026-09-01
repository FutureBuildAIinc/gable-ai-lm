// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
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
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/metrics"
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
	// ListForDate returns every plan for a date, newest first. A date holds
	// more than one plan whenever a re-ingest has superseded another, and
	// nothing forces the one holding live routes to be the latest — so every
	// gate that asks "is this date live?" must ask about all of them.
	ListForDate(ctx context.Context, date string) ([]*Plan, error)
}

// gableSource is the GableLBM integration surface the workflow consumes
// (satisfied by *gable.Client).
type gableSource interface {
	ListOrdersForDate(ctx context.Context, date string) ([]gable.Order, error)
	ListVehicles(ctx context.Context) ([]gable.Vehicle, error)
	ListLocations(ctx context.Context) ([]gable.Location, error)
	ListDrivers(ctx context.Context) ([]gable.Driver, error)
	PushDeliveryRoute(ctx context.Context, route gable.DeliveryRoute) error
	// RecallDeliveryRoute withdraws a route this plan previously pushed. It is
	// the inverse the dispatch board lacked, and without it a re-assignment
	// could only ever orphan the trucks it dropped.
	RecallDeliveryRoute(ctx context.Context, recall gable.RouteRecall) (*gable.RouteRecallResult, error)
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

	// PendingClaimLease bounds how long one plan's PENDING claim on a truck
	// blocks another plan's push for the same date. Zero means
	// defaultPendingClaimLease.
	//
	// It exists because the write-ahead claim is the only thing a crashed
	// process leaves behind, and a claim nothing can ever resolve would wedge
	// the date for good. See Service.claimLease for the full argument.
	PendingClaimLease time.Duration
}

// defaultPendingClaimLease is how long a published-but-unconfirmed claim is
// treated as belonging to a push that is still running.
//
// It is generous relative to a push (a handful of ERP round-trips) and short
// relative to a dispatch day, because both mistakes are recoverable and only
// one is dangerous: too long and a second dispatcher waits, too short and two
// pushes race — and the race is the harm.
const defaultPendingClaimLease = 2 * time.Minute

// claimLease answers "how long may an unresolved PENDING claim hold a truck?"
//
// A PENDING claim is published before the wire call and resolved — confirmed or
// withdrawn — before Push returns, so under ordinary operation this lease is
// never consulted. It exists for the one case that has no in-process answer: a
// process that dies between publishing the claim and resolving it. That claim
// is durable, and nothing in the dead process is coming back to clear it.
//
// The lease is what stops that from wedging the date. Past it, a rival push
// treats the stale claim exactly as it treats a confirmed one — it displaces
// it, tombstones it, and takes the truck — which is sound because the rival's
// own ReplaceDeliveryRoute DELETEs whatever route is upstream for that
// (vehicle, date) anyway. So the worst a crashed push can cost anyone else is
// one lease, and the plan that owns the stale claim can always resolve it
// itself, at once, by pushing again: a PENDING claim never satisfies the
// resume's "already sent" skip, so the route is re-sent and confirmed.
func (s *Service) claimLease() time.Duration {
	if s.cfg.PendingClaimLease > 0 {
		return s.cfg.PendingClaimLease
	}
	return defaultPendingClaimLease
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
	meter   *metrics.Meter
}

func NewService(repo planStore, g gableSource, c catalogSource, f fleetProfiles, rc routeChecker, briefer aiBriefer, cfg Config) *Service {
	return &Service{repo: repo, gable: g, catalog: c, fleet: f, checker: rc, ai: briefer, cfg: cfg}
}

// WithMeter attaches the business-metering counters (pkg/metrics) so this
// module's three value events — a plan created, a truck packed, a route pushed
// — land on /metrics under this deployment's licence labels. cmd/server calls
// it once at boot.
//
// It is a setter rather than a constructor argument because the meter is nil-
// safe by design: a Service built without one meters nothing and behaves
// exactly as it did before the seam existed, which is what makes reverting this
// work a single commit with nothing to unpick.
func (s *Service) WithMeter(m *metrics.Meter) *Service {
	s.meter = m
	return s
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
//
// It also gates the date. A re-ingest ("the day changed, re-run it") used to
// mint a new plan with no reference to — and no gate against — the plan already
// holding that date, so a date whose routes were LIVE on the dealer's dispatch
// board could be re-planned silently. The new plan's recall machinery is scoped
// to its own ledger, which starts empty, so the superseded plan's routes could
// never be withdrawn by anything: exactly the orphan the rest of this work
// removed, reached from the other side. See gateSupersede.
//
// The gate asks about EVERY plan holding the date, not the latest one. A date
// legitimately holds several — that is what "supersede rather than replace"
// means — and nothing makes the live one the newest: a dispatcher can push an
// older plan BY ID after a newer one exists, and a latest-only read then
// reports an empty ledger for a date whose board is live. Three plain HTTP
// calls reached that: ingest, push the first plan, ingest again.
//
// And it asks TWICE. The first ask is the cheap one, before the ERP is touched,
// so a refused re-plan does not pull a day of orders and a catalog to throw
// them away. The second is immediately before the write, because the first read
// happens before three ERP round-trips and a push landing in that window would
// otherwise be planned straight over. The second ask is the authoritative one:
// it is the snapshot that is recalled from and persisted.
func (s *Service) Ingest(ctx context.Context, req IngestRequest) (*Plan, error) {
	if req.Date == "" {
		return nil, fmt.Errorf("%w: date is required", ErrInvalidRequest)
	}
	if _, err := time.Parse("2006-01-02", req.Date); err != nil {
		return nil, fmt.Errorf("%w: invalid date %q; expected YYYY-MM-DD", ErrInvalidRequest, req.Date)
	}

	// Gate BEFORE the ERP is touched. A refused re-ingest is a dispatcher who
	// needs an approver, not a reason to pull a day of orders and a catalog and
	// throw them away. The lookup itself is one indexed read of a table this
	// module owns; it costs the common case nothing upstream.
	superseded, err := s.supersededPlans(ctx, req.Date)
	if err != nil {
		return nil, err
	}
	if err := gateSupersede(superseded, req.Date, req.Override, req.ApprovedBy); err != nil {
		return nil, err
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

	// Branch lookup is best effort and deliberately skipped whenever it cannot
	// change the answer: a request that already named a depot outranks every
	// yard, and a day whose orders name no yard at all has nothing to look up —
	// the branch list would be fetched, handed to the ladder and ignored. That
	// is a round-trip to the ERP per ingest, on exactly the deployments (a
	// GableLBM predating orders.branch_id) least able to answer it. A GableLBM
	// that predates /api/integration/locations answers 404 here; that must
	// degrade to the previous behaviour, not fail every plan for the day.
	orderBranches := branchIDs(analyses)
	var branches []gable.Location
	var branchesErr error
	if (req.DepotLat == nil || req.DepotLng == nil) && len(orderBranches) > 0 {
		if branches, branchesErr = s.gable.ListLocations(ctx); branchesErr != nil {
			slog.Warn("could not list GableLBM branches; falling back down the depot chain",
				"date", req.Date, "branch_ids", orderBranches, "err", branchesErr)
			branches = nil
		}
	}

	depotLat, depotLng, depotSource, depotNote := resolveDepot(req, s.cfg, analyses, branches)
	if branchesErr != nil {
		// The orders DID name a yard — the lookup is not even attempted
		// otherwise; we simply could not read it. Say that, rather than letting
		// the note claim the branch was unknown.
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
	// Re-ask, immediately before the write. The snapshot above was taken BEFORE
	// three ERP round-trips (orders, catalog, branches); a push that landed in
	// that window is invisible to it, and creating a plan on top of a board
	// that gained routes since is the same orphan by a slower route. This
	// second read is the authoritative one — it is what gets recalled from and
	// what gets persisted — and it re-runs the gate, so an unapproved late
	// arrival is refused with the same 423 rather than silently planned over.
	superseded, err = s.supersededPlans(ctx, req.Date)
	if err != nil {
		return nil, err
	}
	if err := gateSupersede(superseded, req.Date, req.Override, req.ApprovedBy); err != nil {
		return nil, err
	}
	// The approval given above is exercised HERE, as late as possible: the
	// superseded plans' routes come off the dealer's board only once this
	// ingest is certain it has a replacement to put there. A recall failure
	// aborts and creates NOTHING — a half-recalled board with a new plan on top
	// is worse than refusing outright.
	if err := s.supersede(ctx, superseded, plan, req.ApprovedBy); err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, plan); err != nil {
		return nil, err
	}
	// Metered only once the plan is STORED. A plan that failed to persist is
	// not a plan, and counting the attempt would bill a dealer for a database
	// error.
	s.meter.PlanCreated()
	return plan, nil
}

// supersededPlans returns every plan a re-ingest of this date would strand:
// the ones whose LEDGER still names routes on the dealer's dispatch board.
//
// All of them, not the latest. A date holds one plan per ingest and the live
// one need not be the newest — a dispatcher can push an older plan by id after
// a re-ingest has already minted a successor — so a latest-only read reports an
// empty ledger for a date whose board is live and lets the next re-ingest orphan
// it. That was reachable in three plain HTTP calls with no concurrency at all.
//
// It keys on the ledger, not the status, and that is deliberate: a plan that
// reached PUSHED and has since had every route recalled has nothing on the
// board, and re-planning that date must stay exactly as frictionless as it is
// today. A date with nothing live anywhere returns an empty slice, and every
// caller below then does nothing — no approval, no recall, no write.
//
// A date with no plans at all is the ordinary first ingest and is not an error.
func (s *Service) supersededPlans(ctx context.Context, date string) ([]*Plan, error) {
	all, err := s.repo.ListForDate(ctx, date)
	if err != nil {
		return nil, fmt.Errorf("look up the existing plans for %s: %w", date, err)
	}
	live := make([]*Plan, 0, len(all))
	for _, p := range all {
		if len(liveRoutes(p)) > 0 {
			live = append(live, p)
		}
	}
	return live, nil
}

// supersede takes the previous plans' routes off the dealer's dispatch board
// before next replaces them, and records on each of those plans both the
// approval and the tombstones.
//
// EVERY live plan for the date, and every live route on each: unlike a
// re-assignment, which keeps the trucks it did not drop, a re-ingest keeps
// nothing. The new plan is built from GableLBM's orders as they now stand and
// has no idea these routes exist, so a route left behind here is left behind
// for good — and a plan left behind here is a plan no future gate can even see,
// because the next re-ingest reads the ledgers this one failed to tombstone.
//
// Ordering is deliberate. Recall first, then persist the tombstones, then let
// the caller create the new plan. Each step can only fail into a state that
// converges on retry: a recall that fails leaves the board and the plan exactly
// as they were; a persist that fails leaves the board clean and the old plan
// still claiming those routes, so the next attempt recalls them again and
// GableLBM answers the idempotent "nothing there" success. Over-reporting what
// is live costs a redundant call. Under-reporting costs a truck loading for a
// run that no longer exists.
//
// The Update is unconditional, even for a plan with nothing left to doom. It is
// not bookkeeping — it is the optimistic-concurrency check. prev was read
// before this ingest committed to anything, and a write that never happens is a
// stale read that never gets caught: the caller would proceed on a snapshot
// somebody else has already moved past. The fast path is exactly where that
// hurts, so the fast path takes the check too. Requirement 5 is untouched
// because a date with nothing live yields an EMPTY prev, and this loop then
// runs zero times.
func (s *Service) supersede(ctx context.Context, prev []*Plan, next *Plan, approvedBy string) error {
	for _, p := range prev {
		doomed := liveRoutes(p)
		if err := s.recallRoutes(ctx, p, doomed, approvedBy,
			fmt.Sprintf("superseded by a re-plan of %s", next.PlanDate),
			"this date cannot be re-planned around it"); err != nil {
			return err
		}
		if err := s.repo.Update(ctx, p); err != nil {
			return fmt.Errorf("record the recall on superseded plan %s: %w", p.ID, err)
		}
		slog.Info("superseded a plan whose routes were live on the dispatch board",
			"superseded_plan", p.ID, "date", p.PlanDate,
			"recalled", len(doomed), "approved_by", approvedBy)
	}
	return nil
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

// branchIDs lists the yards this run's orders ship from. It only projects the
// field out of the workflow's own vocabulary; the semantics — first-appearance
// order, empty ids skipped, duplicates collapsed — live in depot.DistinctBranchIDs
// and are shared with internal/routing, which asks the same question of raw
// gable.Orders. Two implementations of this is how the two modules came to
// disagree about where a run leaves from.
func branchIDs(analyses []OrderAnalysis) []string {
	ids := make([]string, 0, len(analyses))
	for _, a := range analyses {
		ids = append(ids, a.BranchID)
	}
	return depot.DistinctBranchIDs(ids)
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
	if err := gateReshuffle(p, override, approvedBy, planTransitions[actionAssign].gerund); err != nil {
		return nil, err
	}
	// Re-assignment on a plan whose routes are live needs an approver, because
	// it is about to decide which trucks stop existing.
	if err := gateTransition(p, actionAssign, override, approvedBy); err != nil {
		return nil, err
	}
	priorLive := liveRoutes(p)

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

	// The new assignment is now known, so the trucks it DROPPED can be named —
	// and only now. Recall their routes before the plan is rewritten: after
	// this function returns, nothing in the system would remember they were
	// ever live.
	//
	// A recall failure aborts the whole re-assignment and persists nothing. The
	// stored plan still lists every route as live, so the next attempt recalls
	// them all again — which is safe because recall is idempotent upstream, and
	// is the direction to fail in. Over-reporting what is live costs a
	// redundant call; under-reporting leaves a truck loading for a run that no
	// longer exists.
	if err := s.recallDroppedRoutes(ctx, p, priorLive, approvedBy, planTransitions[actionAssign].gerund); err != nil {
		return nil, err
	}

	p.Status = planTransitions[actionAssign].to
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// recallDroppedRoutes withdraws, from GableLBM's dispatch board, every route in
// priorLive whose truck is no longer in p.Loads, and tombstones it on the plan.
//
// It is called AFTER the new assignment is built (the drops cannot be named
// before) and BEFORE the plan is persisted (so a failure leaves the stored plan
// exactly as it was). It returns on the first failure without writing: a
// half-recalled board that the plan has half-forgotten is worse than a stale
// one it still remembers in full.
//
// Trucks that SURVIVE the re-assignment are deliberately not recalled. Their
// routes are stale, not orphaned, and the push upstream is create-or-replace,
// so the next push corrects them. Recalling one would cancel a good route.
func (s *Service) recallDroppedRoutes(ctx context.Context, p *Plan, priorLive []LiveRoute, approvedBy, action string) error {
	kept := make(map[string]bool, len(p.Loads))
	for _, l := range p.Loads {
		kept[l.VehicleID] = true
	}
	doomed := make([]LiveRoute, 0, len(priorLive))
	for _, r := range priorLive {
		if !kept[r.VehicleID] {
			doomed = append(doomed, r)
		}
	}
	return s.recallRoutes(ctx, p, doomed, approvedBy,
		fmt.Sprintf("dropped by %s", action),
		"this plan cannot be re-assigned without it")
}

// recallRoutes is THE recall path: it withdraws each named route from
// GableLBM's dispatch board and tombstones it on the plan that put it there.
//
// It is one function, not one per caller, because "which routes are doomed?" is
// the only thing the callers disagree about. A re-assignment dooms the trucks
// the new assignment drops (recallDroppedRoutes). A re-ingest dooms ALL of
// them, because the plan itself is being replaced and nothing in the new plan
// will remember these routes existed. Everything after that decision — the
// wire call, the idempotent-success reading, the terminal 409, the tombstone,
// the abort-on-first-failure rule — is identical, and a second copy of it is
// how one of the two paths would quietly stop tombstoning.
//
// It returns on the first failure having written nothing further: a
// half-recalled board that the plan has half-forgotten is worse than a stale
// one it still remembers in full. Callers must not persist p when this errors.
//
// note is recorded upstream and on the tombstone ("why did this route vanish").
// blocked completes the one terminal refusal ("...so <blocked>; call the driver
// first"), because a truck already on the road stops a re-assignment and a
// re-plan for different reasons and the dispatcher must read the right one.
func (s *Service) recallRoutes(ctx context.Context, p *Plan, doomed []LiveRoute, approvedBy, note, blocked string) error {
	if len(doomed) == 0 {
		return nil
	}
	now := time.Now()
	for _, r := range doomed {
		if err := s.recallOne(ctx, p, r, approvedBy, note, now); err != nil {
			if errors.Is(err, gable.ErrRouteDispatched) {
				return refusedf("truck %s has already left the yard on this run — its route cannot be recalled, so %s; call the driver first", nameOrID(r), blocked)
			}
			return fmt.Errorf("recall route from GableLBM (truck %s): %w", nameOrID(r), err)
		}
	}
	return nil
}

// recallEach is recallRoutes for the callers that must FINISH THE PASS: it
// attempts every truck and reports the ones it could not take back, instead of
// returning on the first failure.
//
// The two rules are not interchangeable and the difference is which state the
// caller can fall back to. recallRoutes' callers abort and persist NOTHING, so
// a half-recalled board that the plan has half-forgotten is the worst outcome
// and stopping early is right. Its caller here has nothing to fall back to —
// these routes are on the dealer's board with no ledger naming them — and
// stopping at the first truck it cannot withdraw abandons every truck AFTER it
// for a reason that has nothing to do with them. That is the same defect the
// ledger correction had, on the withdrawal path.
func (s *Service) recallEach(ctx context.Context, p *Plan, doomed []LiveRoute, approvedBy, note string) []LiveRoute {
	now := time.Now()
	var failed []LiveRoute
	for _, r := range doomed {
		if err := s.recallOne(ctx, p, r, approvedBy, note, now); err != nil {
			failed = append(failed, r)
		}
	}
	return failed
}

// recallOne withdraws a single route from the dispatch board and tombstones it
// on the plan. It is the shared body of recallRoutes and recallEach, so the two
// abort rules cannot drift apart on anything else — the wire call, the
// idempotent-success reading, the tombstone.
func (s *Service) recallOne(ctx context.Context, p *Plan, r LiveRoute, approvedBy, note string, at time.Time) error {
	res, err := s.gable.RecallDeliveryRoute(ctx, gable.RouteRecall{
		VehicleID:     r.VehicleID,
		ScheduledDate: p.PlanDate,
		Reason:        note,
		RecalledBy:    approvedBy,
	})
	if err != nil {
		slog.Error("could not recall an orphaned route from GableLBM",
			"plan", p.ID, "date", p.PlanDate, "vehicle", r.VehicleName, "vehicle_id", r.VehicleID, "error", err)
		return err
	}
	slog.Info("recalled an orphaned route from the dispatch board",
		"plan", p.ID, "date", p.PlanDate, "vehicle", r.VehicleName,
		"was_live", res.Recalled, "stops", res.StopCount, "approved_by", approvedBy)

	for i := range p.LiveRoutes {
		if p.LiveRoutes[i].VehicleID != r.VehicleID || !p.LiveRoutes[i].Live() {
			continue
		}
		t := at
		p.LiveRoutes[i].RecalledAt = &t
		p.LiveRoutes[i].RecalledBy = approvedBy
		p.LiveRoutes[i].RecallNote = note
	}
	return nil
}

// systemRecaller attributes a ledger correction this module made on its own
// initiative — no dispatcher asked for it and none approved it. It is
// deliberately not an empty string: "who took this route off?" must never read
// as "nobody knows".
const systemRecaller = "gable-ai-lm"

// nameOrID renders a live route for an operator, preferring the truck name.
func nameOrID(r LiveRoute) string {
	if r.VehicleName != "" {
		return r.VehicleName
	}
	return r.VehicleID
}

// --- Step 4: pack every truck (LIFO bundles) ---------------------------------

// Pack 3D-packs every assigned truck: stops load in reverse route order so the
// first delivery is the last material on (rear of bed, first off).
//
// On a locked run, or on one whose routes are already live on the dispatch
// board, it refuses unless override (manual approval) is supplied. It recalls
// nothing: re-packing changes what is ON each truck, never which trucks exist,
// so no route is orphaned. The routes upstream simply become stale manifests,
// and the next push replaces them — the per-load digest guarantees it, because
// a re-pack changes the manifest and therefore the digest.
func (s *Service) Pack(ctx context.Context, id string, override bool, approvedBy string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// NOTE the asymmetry with Assign and Resequence: Pack does NOT consult
	// gateReshuffle. The T2-3 lock freezes a run against re-OPTIMIZATION, and
	// packing is not that — it is the next step of the run the lock froze.
	// A morning run locks at its 06:00 cutoff and the yard still has to pack
	// it; requiring an approver for that would gate the normal path.
	//
	// The live-routes gate below is a different question and does apply: a
	// re-pack of a plan already on the dispatch board supersedes the manifests
	// the yard is working from.
	if err := gateTransition(p, actionPack, override, approvedBy); err != nil {
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

	p.Status = planTransitions[actionPack].to
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	// Every load in the plan was solved above — the loop returns on the first
	// failure, so reaching here means all of them, and the write that stored
	// them succeeded. Re-packing the same plan counts again on purpose: the
	// solver ran again, which is the work being metered.
	s.meter.TrucksPacked(len(p.Loads))
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
	if err := gateReshuffle(p, override, approvedBy, planTransitions[actionResequence].gerund); err != nil {
		return nil, err
	}
	if err := gateTransition(p, actionResequence, override, approvedBy); err != nil {
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
	walkBackAfterReshuffle(p)
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
	// Legal from REVIEWED only. Note that this is NOT what stops a resume: a
	// push that died part-way deliberately left the status at REVIEWED, so
	// re-running it lands here as an ordinary push and skips the trucks that
	// already landed.
	if err := gateTransition(p, actionPush, false, ""); err != nil {
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

	// -----------------------------------------------------------------------
	// WRITE-AHEAD. The claim is published BEFORE the wire call, not after it.
	//
	// This is an ORDERING fix, and it is worth stating why, because three
	// previous attempts corrected the CORRECTION and left the harm reachable.
	//
	// There is no distributed transaction across AI_LM and GableLBM, so there
	// is always a window in which the two systems disagree. The window cannot
	// be removed. It CAN be pointed in the safe direction, and it used to point
	// in the dangerous one:
	//
	//	before  the route is on the dealer's board and NO STORED LEDGER NAMES
	//	        IT. A rival push looking into that window sees nothing to
	//	        correct, takes the truck, and both plans persist a live claim on
	//	        it. That is the orphan and the double claim, in one gap.
	//	after   the truck is CLAIMED and not yet on the board. A rival push
	//	        looking into that window SEES the claim, and either backs off or
	//	        displaces it deliberately.
	//
	// The second is an over-claim, which is the direction this package already
	// chose everywhere else: recallRoutes says it outright — over-reporting
	// what is live costs a redundant, idempotent recall and nothing else, while
	// under-reporting leaves a truck loading for a run that no longer exists.
	//
	// The order below is therefore: reserve, look, write, confirm.
	// -----------------------------------------------------------------------
	now := time.Now()

	// What this attempt will send, decided once, up front. The digest is
	// computed here rather than inside the write loop because the reservation
	// has to know which trucks are in play BEFORE the first wire call, and
	// "which trucks are in play" is exactly "which ones the skip does not
	// cover".
	targets := pushTargets(p)

	// 1. Publish the intent. Every truck this attempt may write is claimed
	//    PENDING and SAVED, so it is visible to another plan's ListForDate from
	//    this moment on. A failure here has written nothing upstream: the plan
	//    is exactly as it was and the dispatcher retries.
	reserved := reserveClaims(p, targets, now)
	if len(reserved) > 0 {
		if err := s.repo.Update(ctx, p); err != nil {
			return nil, fmt.Errorf("reserve the trucks this push is about to write: %w", err)
		}
	}

	// 2. Look, ONCE, and look before writing anything. This read is now a
	//    PRECONDITION rather than a correction, which is the substantive
	//    difference: a push no longer displaces a rival it was unable to check
	//    for. The same answer is used twice — to refuse a contested truck here,
	//    and to correct the ledgers this push displaces after the write loop —
	//    and one read is enough for both because no rival can acquire a claim
	//    inside this window without seeing the reservation above.
	others, listErr := s.repo.ListForDate(ctx, p.PlanDate)
	if listErr != nil {
		// Nothing is known about who else holds these trucks, and — crucially —
		// nothing has been written to the dealer's board yet. Handing the
		// reservation back is therefore free, and it is the only answer that
		// cannot leave two plans holding one truck. This is the branch that
		// used to persist the pushing plan WITH the claim it had just failed to
		// make exclusive.
		s.releaseReservation(ctx, p, reserved, now,
			fmt.Sprintf("released: the other plans for %s could not be read, so this push could not tell whether another plan holds this truck", p.PlanDate))
		return nil, fmt.Errorf("look up the other plans for %s: %w", p.PlanDate, listErr)
	}

	// 3. Refuse a truck another push is holding right now.
	//
	//    Both racers publish before they look, so at most one of them can look
	//    and see nothing: for neither to see the other, each would have to have
	//    looked before the other wrote, and that orders the four events in a
	//    cycle. The one that sees a rival intent therefore stands down. Both
	//    standing down is possible and is harmless — nothing is on the board
	//    and the dispatcher pushes again.
	if held := contestedClaims(others, p.ID, targets, now, s.claimLease()); len(held) > 0 {
		s.releaseReservation(ctx, p, reserved, now,
			"released: another plan for this date published a claim on this truck first")
		return nil, fmt.Errorf("%w — another push for %s is already holding %s, and nothing was written to the dispatch board",
			ErrVersionConflict, p.PlanDate, strings.Join(held, ", "))
	}

	// The write loop is per-truck and every truck is acknowledged on the plan
	// as it lands. That acknowledgement is the whole difference between a push
	// that can be resumed and one that cannot: this loop used to return on the
	// first failure with the status untouched, so a push that died on truck 3
	// of 5 left trucks 1 and 2 live on the dealer's board, the plan reading
	// REVIEWED, and NO record anywhere of which routes had actually been
	// written. Nothing was resumable and nothing was recallable.
	var (
		pushErr     error
		failedTruck string
		wrote       []string
		acks        []pushAck
		skipped     []string
	)
	for _, tg := range targets {
		l := &p.Loads[tg.load]

		// Already on the board, byte for byte. Skipping is what makes a resume
		// cheap, and the digest — not the timestamp — is what makes it safe:
		// any change to stops, driver or manifest since the last push produces
		// a different digest and re-sends.
		//
		// The LEDGER has the last word, though, and it now has to say CONFIRMED
		// and not merely "claimed". A digest records what this plan once sent;
		// a PENDING claim records that this plan published an intent and cannot
		// prove the wire call happened, so it always re-sends. And
		// clearDisplacedClaims tombstones the entry outright when another
		// plan's push destroys the route (the ERP keeps at most one
		// non-dispatched route per truck per day). Skipping on the digest alone
		// would leave that truck with no route at all and a plan convinced it
		// had sent one.
		if tg.skip {
			skipped = append(skipped, l.VehicleName)
			continue
		}

		if err := s.gable.PushDeliveryRoute(ctx, tg.route); err != nil {
			pushErr, failedTruck = err, l.VehicleName
			break
		}

		t := now
		l.PushedAt = &t
		l.PushedDigest = tg.digest
		// 4. Confirm: PENDING -> LIVE, on the claim already published above.
		confirmClaim(p, l.VehicleID, l.VehicleName, now)
		wrote = append(wrote, l.VehicleName)
		acks = append(acks, pushAck{vehicleID: l.VehicleID, vehicleName: l.VehicleName, digest: tg.digest})
		// Metered per route, inside the loop, because that is where the value
		// actually occurs: a push that fails on the fourth truck still put
		// three real routes on the dealer's dispatch board, and they do not
		// un-happen because the fifth call errored.
		s.meter.RoutesPushed(1)
	}

	if pushErr != nil {
		// The status is NOT advanced below when this is set: the plan's status
		// must describe the dispatch board, and the board is only partly
		// written. Leaving it at REVIEWED is also what makes the retry a
		// legal, ordinary push.
		slog.Error("push to GableLBM failed part-way through the run",
			"plan", p.ID, "date", p.PlanDate, "failed_truck", failedTruck,
			"written", len(wrote), "of", len(p.Loads), "error", pushErr)
	}

	if len(skipped) > 0 {
		slog.Info("resumed a partial push", "plan", p.ID, "date", p.PlanDate,
			"skipped_already_live", strings.Join(skipped, ", "), "written", len(wrote))
	}

	// Keep the ledger truthful at the source. See clearDisplacedClaims: every
	// truck this plan has CONFIRMED has destroyed whatever route another plan
	// for this date had on that truck, and that plan's ledger is now lying.
	//
	// The set is every truck THIS PLAN NOW HOLDS CONFIRMED, and deliberately
	// not the trucks this attempt happened to write. The two differ on exactly
	// the path that used to defeat the correction: a push that died part-way,
	// resumed, and SKIPPED the trucks that already landed — so the correction
	// owed for one of them was invisible to the only attempt left that could
	// pay it. It is not the reserved set either: a truck this push claimed and
	// never managed to write has displaced nobody.
	uncleared, clearErr := s.clearDisplacedClaims(ctx, p, others, confirmedVehicleIDs(p), now)
	if clearErr != nil {
		slog.Error("a push could not correct every ledger it displaced",
			"plan", p.ID, "date", p.PlanDate,
			"uncleared", strings.Join(uncleared, ", "), "error", clearErr)
	}

	// A correction this push abandoned must not leave the push asserting what
	// it failed to make true. Giving up THIS plan's claim on a truck whose
	// rival claim still stands keeps the date to one claimant per truck, and
	// costs nothing upstream: the route on the board is ours, and the rival
	// ledger naming it is the one that survives. The load keeps its push ack,
	// so the next resume reads Confirmed == false, re-sends the route and
	// tries the correction again.
	//
	// abandoned is the other half of the write-ahead bargain: a truck claimed
	// as intent and then never written must hand the claim straight back, or
	// the over-claim this ordering deliberately accepts inside the window would
	// outlive the window.
	out := &pushOutcome{
		acks:       acks,
		retracted:  uncleared,
		abandoned:  unwritten(reserved, acks, uncleared),
		fromStatus: p.Status,
		status:     p.Status,
		at:         now,
	}
	if pushErr == nil && clearErr == nil {
		out.status = planTransitions[actionPush].to
	}
	out.applyTo(p)

	persistErr := s.persistPush(ctx, p, out)

	switch {
	case pushErr != nil:
		// Never mask the push failure with a bookkeeping one — this is the
		// sentence the dispatcher has to act on. The bookkeeping failures are
		// logged above and inside persistPush, and neither is allowed to leave
		// the board and the ledgers disagreeing.
		return nil, refusedf("push stopped at truck %s: %d of %d truck(s) are now live on the dispatch board (%s). GableLBM was not reachable for the rest — run push again to finish the run; the trucks already written are skipped",
			failedTruck, len(confirmedVehicleIDs(p)), len(p.Loads), strings.Join(wrote, ", "))
	case clearErr != nil:
		return nil, clearErr
	case persistErr != nil:
		return nil, persistErr
	}
	return p, nil
}

// pushTarget is one truck's share of a push attempt, decided before any wire
// call so the reservation and the write loop cannot disagree about which trucks
// are in play.
type pushTarget struct {
	load   int // index into Plan.Loads
	route  gable.DeliveryRoute
	digest string
	skip   bool // already on the board, byte for byte, and CONFIRMED
}

// pushTargets renders every load as what this attempt would send it, and marks
// the ones the resume skip already covers.
func pushTargets(p *Plan) []pushTarget {
	out := make([]pushTarget, 0, len(p.Loads))
	for i := range p.Loads {
		l := &p.Loads[i]
		route := deliveryRoute(p, *l)
		digest := routeDigest(route)
		out = append(out, pushTarget{
			load:   i,
			route:  route,
			digest: digest,
			skip:   l.PushedAt != nil && l.PushedDigest == digest && routeIsConfirmed(p, l.VehicleID),
		})
	}
	return out
}

// reserveClaims publishes this attempt's intent to hold every truck it may
// write, and reports the trucks whose claim is PENDING as a result.
//
// A truck this plan already holds CONFIRMED is left alone: the route really is
// on the board, and demoting a confirmed claim to an intent would throw away
// the one fact that makes it recallable. A truck already PENDING — this plan's
// own earlier attempt, or a process that died mid-push — is returned in the set
// anyway, because this attempt is now the thing that has to resolve it.
func reserveClaims(p *Plan, targets []pushTarget, at time.Time) []string {
	var reserved []string
	for _, tg := range targets {
		if tg.skip {
			continue
		}
		l := p.Loads[tg.load]
		claim := heldClaim(p, l.VehicleID)
		switch {
		case claim == nil:
			t := at
			p.LiveRoutes = append(p.LiveRoutes, LiveRoute{
				VehicleID: l.VehicleID, VehicleName: l.VehicleName,
				State: ClaimPending, ClaimedAt: &t,
			})
			reserved = append(reserved, l.VehicleID)
		case claim.Pending():
			claim.VehicleName = l.VehicleName
			reserved = append(reserved, l.VehicleID)
		}
	}
	return reserved
}

// namesVehicle reports whether this plan's ledger mentions a truck at all,
// tombstones included. It is the "has this chapter been closed?" reading, as
// against heldClaim's "is it open?".
func namesVehicle(p *Plan, vehicleID string) bool {
	for _, r := range p.LiveRoutes {
		if r.VehicleID == vehicleID {
			return true
		}
	}
	return false
}

// heldClaim returns this plan's live (PENDING or LIVE) ledger entry for a
// truck, or nil. It is a pointer into p.LiveRoutes so callers can promote it.
func heldClaim(p *Plan, vehicleID string) *LiveRoute {
	for i := range p.LiveRoutes {
		if p.LiveRoutes[i].VehicleID == vehicleID && p.LiveRoutes[i].Live() {
			return &p.LiveRoutes[i]
		}
	}
	return nil
}

// contestedClaims names the trucks this push must NOT take, because another
// plan for the date has published an intent on them that is still within its
// lease. It returns operator-facing truck names, sorted, so the refusal reads
// the same every time.
//
// A rival's CONFIRMED claim is not contested. That is the ordinary
// displacement this module has always handled: the push replaces the route
// upstream and clearDisplacedClaims corrects the ledger afterwards. An EXPIRED
// pending claim is not contested either — see Service.claimLease for why that
// is what stops a dead process wedging the date.
func contestedClaims(others []*Plan, selfID string, targets []pushTarget, now time.Time, lease time.Duration) []string {
	wanted := map[string]bool{}
	for _, tg := range targets {
		if !tg.skip {
			wanted[tg.route.VehicleID] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, other := range others {
		if other.ID == selfID {
			continue
		}
		for _, r := range other.LiveRoutes {
			if !r.Pending() || !wanted[r.VehicleID] || seen[r.VehicleID] {
				continue
			}
			if r.ClaimedAt != nil && now.Sub(*r.ClaimedAt) >= lease {
				continue
			}
			seen[r.VehicleID] = true
			out = append(out, nameOrID(r))
		}
	}
	sort.Strings(out)
	return out
}

// releaseReservation hands back claims this push published and then decided not
// to use, on an exit that wrote NOTHING to the dealer's board.
//
// It is best-effort by construction and that is safe in this one direction: a
// release that fails to persist leaves an over-claim, which blocks other plans
// for at most one lease and is cleared by this plan's own next push. A release
// that was never attempted would leave it there indefinitely.
func (s *Service) releaseReservation(ctx context.Context, p *Plan, reserved []string, at time.Time, note string) {
	if len(reserved) == 0 {
		return
	}
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		for _, id := range reserved {
			retractClaim(p, id, vehicleNameFor(p, id), at, note)
		}
		if err = s.repo.Update(ctx, p); err == nil {
			return
		}
		if !errors.Is(err, ErrVersionConflict) || i == attempts-1 {
			break
		}
		fresh, gerr := s.repo.Get(ctx, p.ID)
		if gerr != nil {
			err = gerr
			break
		}
		*p = *fresh
	}
	slog.Error("could not hand back a push reservation — the trucks stay claimed until this plan pushes again or the claim lease runs out",
		"plan", p.ID, "date", p.PlanDate, "trucks", strings.Join(reserved, ", "),
		"lease", s.claimLease(), "error", err)
}

// unwritten is the reserved trucks this attempt never put on the board, and
// therefore the claims it has to give straight back.
//
// Trucks whose rival correction failed are excluded: those are already handed
// back through pushOutcome.retracted, with the note that says why, and
// tombstoning one twice with two different reasons would make "who stopped
// claiming this, and what for" unanswerable.
func unwritten(reserved []string, acks []pushAck, retracted []string) []string {
	done := make(map[string]bool, len(acks)+len(retracted))
	for _, a := range acks {
		done[a.vehicleID] = true
	}
	for _, id := range retracted {
		done[id] = true
	}
	out := make([]string, 0, len(reserved))
	for _, id := range reserved {
		if !done[id] {
			out = append(out, id)
		}
	}
	return out
}

// pushAck is one truck this push attempt wrote to GableLBM, with the digest
// recording what was sent. It is the unit the ledger is rebuilt from when the
// save has to be replayed onto fresh state.
type pushAck struct {
	vehicleID   string
	vehicleName string
	digest      string
}

// pushOutcome is everything one Push attempt decided, in a form that can be
// re-stated on a freshly read copy of the plan.
//
// It exists because the save is version-checked and the plan has been out of
// the caller's hands for several ERP round-trips. A concurrent write means the
// outcome has to be replayed rather than abandoned, and replaying it from the
// plan object this call already mutated would replay it over state somebody
// else has legitimately moved past.
type pushOutcome struct {
	acks      []pushAck // trucks this attempt wrote upstream
	retracted []string  // vehicle ids whose displaced rival claim could not be cleared
	abandoned []string  // vehicle ids claimed as intent and never written
	// disowned is filled in BY applyTo: trucks whose published claim was gone
	// from the plan by the time the outcome was replayed, because another plan
	// displaced them. They are not re-asserted and not withdrawn from the
	// board — the plan that took them over is the one that names them now.
	disowned []string
	// fromStatus is the status the push's gates were evaluated against, and
	// status is the one those gates earned. They are kept apart because the
	// two halves of this outcome replay differently: the LEDGER is a record of
	// what is on the dealer's board and is true whatever else has happened to
	// the plan, while the transition is a decision, and a decision made
	// against a status somebody has since changed — a concurrent re-pack
	// invalidating the review, say — has to be re-earned, not replayed.
	fromStatus string
	status     string
	at         time.Time
}

// applyTo re-states this outcome on p. It is idempotent, which is what lets the
// first save and every retry write the same thing.
//
// It reports whether the transition was still applicable. False means the
// ledger has been re-stated — that part is never given up — but the plan had
// moved to a status this push never evaluated, so its status is left exactly
// as the other writer set it.
//
// A REPLAY MUST NOT RESURRECT A CLAIM THAT IS NO LONGER THIS PLAN'S TO MAKE.
// The claim for every truck here was published on the plan before the wire
// call, so on a fresh copy it is either still held by this plan or it has been
// TOMBSTONED by somebody who displaced it. In the second case the truck now
// belongs to another plan, whose ledger names the route on the board;
// re-asserting it would put the date back to two plans holding one truck —
// which is exactly the state a version conflict used to save us from, before
// the replay was introduced. It is dropped instead, and recorded in disowned so
// the caller does not go on to withdraw a route somebody else is relying on.
func (o *pushOutcome) applyTo(p *Plan) bool {
	o.disowned = nil
	for _, a := range o.acks {
		// TOMBSTONED, not merely missing. The two are different facts and only
		// one of them means "not ours any more": a tombstone is somebody
		// writing down that they displaced this claim, whereas an entry that is
		// absent altogether is a reservation that never reached the store, and
		// its route IS on the dealer's board and must be named by somebody.
		if heldClaim(p, a.vehicleID) == nil && namesVehicle(p, a.vehicleID) {
			o.disowned = append(o.disowned, a.vehicleID)
			continue
		}
		confirmClaim(p, a.vehicleID, a.vehicleName, o.at)
		for i := range p.Loads {
			if p.Loads[i].VehicleID != a.vehicleID {
				continue
			}
			t := o.at
			p.Loads[i].PushedAt = &t
			p.Loads[i].PushedDigest = a.digest
		}
	}
	note := fmt.Sprintf("another plan for %s still claims this truck and its ledger could not be corrected", p.PlanDate)
	for _, id := range o.retracted {
		retractClaim(p, id, vehicleNameFor(p, id), o.at, note)
	}
	for _, id := range o.abandoned {
		retractClaim(p, id, vehicleNameFor(p, id), o.at,
			"claimed ahead of the push and never written to the dispatch board")
	}
	if p.Status != o.fromStatus {
		return false
	}
	p.Status = o.status
	return true
}

// retractClaim gives up this plan's claim on one truck and leaves a tombstone
// saying why, so "who stopped claiming this and when" is answerable. It is
// idempotent: an entry this same push already tombstoned is rewritten, never
// duplicated, and an older closed chapter for the same truck is left alone.
func retractClaim(p *Plan, vehicleID, vehicleName string, at time.Time, note string) {
	for i := range p.LiveRoutes {
		r := &p.LiveRoutes[i]
		if r.VehicleID != vehicleID {
			continue
		}
		// Reuse the entry this same call already tombstoned (a replay), and
		// leave an OLDER closed chapter for the same truck alone. The key is
		// the tombstone's own timestamp: a PENDING claim has no PushedAt to
		// compare, so keying on that one silently appended a duplicate
		// tombstone on every replay.
		if !r.Live() && !(r.RecalledAt != nil && r.RecalledAt.Equal(at)) {
			continue
		}
		t := at
		r.RecalledAt = &t
		r.RecalledBy = systemRecaller
		r.RecallNote = note
		return
	}
	t := at
	p.LiveRoutes = append(p.LiveRoutes, LiveRoute{
		VehicleID: vehicleID, VehicleName: vehicleName, PushedAt: at,
		RecalledAt: &t, RecalledBy: systemRecaller, RecallNote: note,
	})
}

// vehicleNameFor renders a truck for an operator from whatever the plan knows.
func vehicleNameFor(p *Plan, vehicleID string) string {
	for _, l := range p.Loads {
		if l.VehicleID == vehicleID {
			return l.VehicleName
		}
	}
	for _, r := range p.LiveRoutes {
		if r.VehicleID == vehicleID && r.VehicleName != "" {
			return r.VehicleName
		}
	}
	return vehicleID
}

// persistPush saves what a push actually did, and does not let a concurrent
// edit throw it away.
//
// p was read at the top of Push and has since crossed several ERP round-trips.
// ANY other write to the same plan inside that window — a lock, an unlock, a
// proof, a sign-off, a priority or dimension change, a resequence, an assign,
// a pack — bumps its version, so `UPDATE ... WHERE version=$n` affects zero
// rows. Returning that conflict straight to the caller, which is what this
// used to do, discarded the ledger naming every route the push had ALREADY put
// on the dealer's board: routes live upstream that no plan's ledger names,
// which gateSupersede cannot see and no recall path can reach. That is the
// orphan this whole branch exists to remove, reached with no ERP failure and
// no second plan involved.
//
// A ledger append is not a conflicting edit and must not be lost to one, so a
// conflict re-reads and re-states the same outcome on fresh state — the answer
// tombstoneDisplaced already models. The other writer's edit survives, because
// the replay only re-states this push's own decisions.
//
// What is replayed is only the ledger and the transition this push earned, and
// the transition only while the plan is still at the status that earned it: a
// concurrent re-pack that invalidated the review must not come back PUSHED.
// When that happens the ledger is still saved — the routes really are on the
// board and something has to be able to recall them — and the conflict is
// returned so the dispatcher reloads and pushes again.
//
// A conflict that will not clear is the one case where the board cannot be
// recorded at all, and there the routes come back off: see withdrawUnrecorded.
func (s *Service) persistPush(ctx context.Context, p *Plan, out *pushOutcome) error {
	const attempts = 3
	var err error
	current := true
	for i := 0; i < attempts; i++ {
		if err = s.repo.Update(ctx, p); err == nil {
			if current {
				return nil
			}
			slog.Warn("recorded a push onto a plan that had already moved on — the ledger is safe, the transition is not",
				"plan", p.ID, "date", p.PlanDate, "status", p.Status, "wanted", out.status)
			return ErrVersionConflict
		}
		if !errors.Is(err, ErrVersionConflict) || i == attempts-1 {
			break
		}
		fresh, gerr := s.repo.Get(ctx, p.ID)
		if gerr != nil {
			err = gerr
			break
		}
		current = out.applyTo(fresh)
		*p = *fresh
	}
	// The routes this push wrote were claimed on this plan and SAVED before the
	// wire call, so this failure is a lost CONFIRMATION, not a lost ledger:
	// they are still named, still recallable and still visible to every gate,
	// and the dispatcher's retry re-sends them (a PENDING claim never satisfies
	// the resume skip) and confirms them. withdrawUnrecorded therefore finds
	// nothing to do unless that reservation is genuinely absent from the store.
	slog.Error("could not record a push — its claims stay published so the routes it wrote are still named",
		"plan", p.ID, "date", p.PlanDate, "routes", len(out.acks), "error", err)
	s.withdrawUnrecorded(ctx, p, out)
	return err
}

// withdrawUnrecorded takes back off the dispatch board the routes THIS attempt
// wrote that NO ledger anywhere names.
//
// Write-ahead changes what that set is, and almost always empties it. Every
// truck this push wrote was claimed on this plan and SAVED before the wire call,
// so a save that later failed still leaves the routes named, recallable and
// visible to gateSupersede: there is nothing to withdraw, and withdrawing
// anyway would take routes off the dealer's board while a stored claim still
// points at them — the mirror lie, from the other side. So the set is computed
// from what the store actually holds, not assumed.
//
// It is not empty in one case, and that case is why this survives: a plan whose
// reservation is not in the store when we look — it never landed, or the row is
// gone. Then the routes really are unnameable, nothing can ever recall them,
// and the alternative is leaving the dealer dispatching trucks this system
// cannot see.
//
// Trucks whose claim this push gave up (see pushOutcome.retracted) and trucks
// another plan took over inside the window (pushOutcome.disowned) are left
// alone: another plan's ledger names those routes, so they are not orphans, and
// recalling one would cancel a route that plan is relying on.
func (s *Service) withdrawUnrecorded(ctx context.Context, p *Plan, out *pushOutcome) {
	spoken := make(map[string]bool, len(out.retracted)+len(out.disowned))
	for _, id := range out.retracted {
		spoken[id] = true
	}
	for _, id := range out.disowned {
		spoken[id] = true
	}

	stored, err := s.repo.Get(ctx, p.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		slog.Error("could not read back a push that failed to record itself — leaving the routes on the board rather than withdraw ones a stored claim may still name",
			"plan", p.ID, "date", p.PlanDate, "error", err)
		return
	}

	doomed := make([]LiveRoute, 0, len(out.acks))
	for _, a := range out.acks {
		if spoken[a.vehicleID] {
			continue
		}
		if stored != nil && heldClaim(stored, a.vehicleID) != nil {
			continue // the reservation is in the store: this route is named
		}
		doomed = append(doomed, LiveRoute{VehicleID: a.vehicleID, VehicleName: a.vehicleName, PushedAt: out.at})
	}
	if len(doomed) == 0 {
		return
	}
	slog.Error("withdrawing routes a push wrote that no ledger anywhere names",
		"plan", p.ID, "date", p.PlanDate, "routes", len(doomed))
	if failed := s.recallEach(ctx, p, doomed, systemRecaller,
		"withdrawn: this push could not be recorded"); len(failed) > 0 {
		names := make([]string, 0, len(failed))
		for _, r := range failed {
			names = append(names, nameOrID(r))
		}
		slog.Error("could not withdraw every route of a push that was never recorded — the dispatch board is ahead of every ledger for this date",
			"plan", p.ID, "date", p.PlanDate, "stranded", strings.Join(names, ", "))
	}
}

// clearDisplacedClaims tombstones every OTHER plan's ledger entry for a truck
// this push has just re-routed on this date.
//
// It exists because of a fact upstream: GableLBM's ReplaceDeliveryRoute DELETEs
// any DRAFT/SCHEDULED delivery_route for the same (vehicle_id, scheduled_date)
// before inserting. The ERP therefore holds AT MOST ONE non-dispatched route
// per truck per day — two plans on one date both claiming a truck live is not
// untidy, it is a state the ERP cannot represent. The second push has ALREADY
// destroyed the first plan's route; the only question is whether this system
// notices.
//
// Until it did, the older plan's ledger kept naming a route that no longer
// existed, and everything keyed on that ledger inherited the lie: the recall
// path would cancel a route belonging to somebody else (the wire key is
// (vehicle, date), not "the route this plan pushed"), and gateSupersede would
// refuse future re-ingests over ghosts. The ledger is a MIRROR of the dispatch
// board; a mirror that can silently diverge makes every gate above unsound.
//
// This is NOT a second recall path. Nothing is withdrawn from GableLBM here —
// there is nothing left to withdraw — so there is no wire call, no dispatched
// 409 and no approval to weigh. It only stops a ledger claiming what upstream
// no longer has.
//
// It returns the vehicle ids it could NOT clear, alongside the first failure.
// The caller needs both: a correction that was abandoned leaves a rival claim
// standing, and the push must then give up its own claim on that truck rather
// than leave the date with two plans holding one. It also keeps going after a
// failure instead of returning on the first one, so a second displaced ledger
// is not left lying for a reason that has nothing to do with it.
//
// It is handed the date's plans rather than reading them itself, because the
// read now has to happen BEFORE the wire call — see Push. That is not a
// refactor: a correction that reads afterwards can only ever apologise for a
// displacement it has already committed, and the read failing is what used to
// leave the pushing plan holding a claim it had just failed to make exclusive.
// Reading once, up front, makes the same answer serve as the precondition and
// as the correction, and no rival can take a claim inside the window without
// seeing this push's own published intent.
func (s *Service) clearDisplacedClaims(ctx context.Context, p *Plan, others []*Plan, vehicleIDs []string, at time.Time) ([]string, error) {
	if len(vehicleIDs) == 0 {
		return nil, nil
	}
	displaced := make(map[string]bool, len(vehicleIDs))
	for _, id := range vehicleIDs {
		displaced[id] = true
	}
	var uncleared []string
	var firstErr error
	for _, other := range others {
		if other.ID == p.ID || !claimsAnyOf(other, vehicleIDs) {
			continue
		}
		if err := s.tombstoneDisplaced(ctx, other.ID, p, displaced, at); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			uncleared = append(uncleared, claimedFrom(other, vehicleIDs)...)
		}
	}
	return uncleared, firstErr
}

// claimedFrom is claimsAnyOf's answer spelled out: which of these trucks does
// this plan still claim?
func claimedFrom(p *Plan, vehicleIDs []string) []string {
	out := make([]string, 0, len(vehicleIDs))
	for _, id := range vehicleIDs {
		if routeIsLive(p, id) {
			out = append(out, id)
		}
	}
	return out
}

// tombstoneDisplaced re-reads one plan and marks the displaced trucks recalled.
//
// It re-reads rather than writing the copy it was handed because the copy is
// already a moment old and Update is version-checked: another actor saving that
// plan in between must not turn a correctness fix into a 409 the dispatcher
// cannot act on. A conflict therefore retries on fresh state; a conflict that
// keeps happening is surfaced, because a ledger left lying is the defect.
//
// Giving up is not the end of it. Push used to return that 409 having already
// persisted itself with the very claim this failed to make exclusive — the
// abandoned correction left behind exactly the double claim it was called to
// remove. The caller now gives up its own claim on those trucks instead; see
// clearDisplacedClaims' return and pushOutcome.retracted.
func (s *Service) tombstoneDisplaced(ctx context.Context, id string, by *Plan, displaced map[string]bool, at time.Time) error {
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		var other *Plan
		if other, err = s.repo.Get(ctx, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return fmt.Errorf("re-read plan %s to correct its ledger: %w", id, err)
		}
		n := 0
		for j := range other.LiveRoutes {
			r := &other.LiveRoutes[j]
			if !r.Live() || !displaced[r.VehicleID] {
				continue
			}
			t := at
			r.RecalledAt = &t
			r.RecalledBy = systemRecaller
			r.RecallNote = fmt.Sprintf("replaced on the dispatch board by plan %s", by.ID)
			n++
		}
		if n == 0 {
			return nil
		}
		if err = s.repo.Update(ctx, other); err == nil {
			slog.Info("corrected a plan whose routes this push replaced upstream",
				"plan", other.ID, "date", other.PlanDate, "replaced_by", by.ID, "routes", n)
			return nil
		}
		if !errors.Is(err, ErrVersionConflict) {
			return fmt.Errorf("correct the ledger of plan %s: %w", id, err)
		}
	}
	slog.Error("could not correct a plan whose routes this push replaced upstream",
		"plan", id, "date", by.PlanDate, "replaced_by", by.ID, "error", err)
	return err
}

// routeIsLive reports whether this plan's ledger still claims this truck's
// route is on the dispatch board. It is the per-truck reading of liveRoutes,
// which stays the one place that decides what "live" means.
func routeIsLive(p *Plan, vehicleID string) bool {
	for _, r := range liveRoutes(p) {
		if r.VehicleID == vehicleID {
			return true
		}
	}
	return false
}

// claimsAnyOf is routeIsLive over a set, so the pre-check that saves a read
// cannot drift from the check that does the work.
func claimsAnyOf(p *Plan, vehicleIDs []string) bool {
	for _, id := range vehicleIDs {
		if routeIsLive(p, id) {
			return true
		}
	}
	return false
}

// deliveryRoute builds the GableLBM write-back payload for one truck.
func deliveryRoute(p *Plan, l TruckLoad) gable.DeliveryRoute {
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
	return route
}

// routeDigest fingerprints exactly what would be written upstream for one
// truck, so a resumed push can tell "already sent, unchanged" from "sent, but
// the plan has moved on since".
//
// It hashes the marshalled payload rather than a hand-picked set of fields
// because the risk it guards is precisely the field somebody forgets to add:
// encoding/json sorts map keys, so the manifest hashes stably too.
func routeDigest(route gable.DeliveryRoute) string {
	raw, err := json.Marshal(route)
	if err != nil {
		// Unreachable for this payload, and a digest that cannot be computed
		// must never read as "matches" — an empty digest never equals a stored
		// one, so the route is re-pushed.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// confirmClaim promotes this plan's published claim on a truck from PENDING to
// LIVE: the route is now known to be on the dispatch board.
//
// It is the second half of the write-ahead pair and it is idempotent, so the
// first save and every replay of it write the same thing. The append is a
// fallback for a claim that was never reserved — Push always reserves first, so
// it should not fire, but a confirmed claim that failed to record itself would
// be an orphan and this is the cheaper of the two mistakes.
func confirmClaim(p *Plan, vehicleID, vehicleName string, at time.Time) {
	if r := heldClaim(p, vehicleID); r != nil {
		r.State = ClaimLive
		r.PushedAt = at
		r.VehicleName = vehicleName
		return
	}
	p.LiveRoutes = append(p.LiveRoutes, LiveRoute{
		VehicleID: vehicleID, VehicleName: vehicleName, State: ClaimLive, PushedAt: at,
	})
}

// routeIsConfirmed reports whether this plan's ledger says this truck's route
// is KNOWN to be on the dispatch board — not merely claimed ahead of a wire
// call that may never have happened. It is the reading the resume skip needs.
func routeIsConfirmed(p *Plan, vehicleID string) bool {
	for _, r := range p.LiveRoutes {
		if r.VehicleID == vehicleID && r.Confirmed() {
			return true
		}
	}
	return false
}

// confirmedVehicleIDs is the set of trucks whose route this plan KNOWS it put
// on the dispatch board, and therefore the set whose rival claims it has
// displaced. A truck merely claimed and never written has displaced nobody.
func confirmedVehicleIDs(p *Plan) []string {
	out := make([]string, 0, len(p.LiveRoutes))
	for _, r := range p.LiveRoutes {
		if r.Confirmed() {
			out = append(out, r.VehicleID)
		}
	}
	return out
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
	if err := gateReshuffle(p, override, approvedBy, planTransitions[actionPriority].gerund); err != nil {
		return nil, err
	}
	if err := gateTransition(p, actionPriority, override, approvedBy); err != nil {
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
		walkBackAfterReshuffle(p)
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
	// A dimension override re-packs the truck carrying the order, so it is a
	// reshuffle like any other and answers to both gates. It previously had
	// neither: it was the one mutation that could silently re-pack a locked,
	// pushed run.
	if err := gateReshuffle(p, req.Override, req.ApprovedBy, planTransitions[actionDimensions].gerund); err != nil {
		return nil, err
	}
	if err := gateTransition(p, actionDimensions, req.Override, req.ApprovedBy); err != nil {
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
	walkBackAfterReshuffle(p)
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
