// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/catalog"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/fleet"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// In-memory doubles for the workflow's five seams plus its plan store, so the
// orchestrator can be exercised end-to-end with no Postgres and no live ERP.

// fakePlanStore reproduces the Postgres persistence contract in memory:
// every read hands back an independent copy (the real repository round-trips
// through JSONB, so callers can never alias stored state), and every write is
// guarded by the plan's optimistic-concurrency version exactly as
// `UPDATE ... WHERE id=$1 AND version=$expected` is.
type fakePlanStore struct {
	mu    sync.Mutex
	plans map[string]*Plan
	// created is the insertion order of plan ids, so GetLatestForDate can mean
	// LATEST — the repository orders by created_at DESC, and map iteration
	// order does not. It matters now that a date can legitimately hold more
	// than one plan (a re-ingest supersedes rather than replaces).
	created []string
	nextID  int

	updates   int
	conflicts int

	// afterGet fires ONCE, immediately after the next Get returns.
	//
	// It exists for one window that has no other seam: Push reads the plan
	// UNLOCKED to discover which date to lock, then re-reads it under the lock
	// and gates on that. Landing a competing writer between those two reads is
	// the only way to tell the two apart, and telling them apart is the whole
	// point of the second read.
	afterGet func()

	// dateLocks is the set of dates currently held by a WithDateLock caller;
	// lockGrants/lockRefusals count the two outcomes.
	dateLocks    map[string]bool
	lockGrants   int
	lockRefusals int

	// beforeUpdate fires once, immediately before the next Update is applied,
	// letting a test land a competing writer inside another actor's
	// read-modify-write window (two dispatch users, two goroutines, one plan).
	beforeUpdate func()

	// listCalls counts ListForDate calls and afterListForDate fires
	// immediately AFTER each one returns, with that count.
	//
	// It exists for one window with no other seam. A re-plan reads which plans
	// hold the date and then, several steps later, recalls what that read
	// named. Whether the read was taken INSIDE the date claim or just before
	// it is invisible to any end-state assertion — both orderings converge
	// whenever nothing lands in between — and it is the whole difference
	// between a recall set that is authoritative and one that is merely
	// recent. A hook here lets a test land a competing push in exactly that
	// gap and watch whether the claim refuses it.
	listCalls        int
	afterListForDate func(n int)
}

func newFakePlanStore(seed ...*Plan) *fakePlanStore {
	s := &fakePlanStore{plans: map[string]*Plan{}}
	for _, p := range seed {
		// nextID advances for EVERY seeded plan, named or not. It used to
		// advance only for unnamed ones, so a store seeded with "plan-1" then
		// minted "plan-1" again on the first Create and silently overwrote the
		// seed — invisible until a date could legitimately hold two plans.
		s.nextID++
		if p.ID == "" {
			p.ID = fmt.Sprintf("plan-%d", s.nextID)
		}
		if p.Version == 0 {
			p.Version = 1
		}
		s.plans[p.ID] = clonePlan(p)
		s.created = append(s.created, p.ID)
	}
	return s
}

func clonePlan(p *Plan) *Plan {
	raw, err := json.Marshal(p)
	if err != nil {
		panic("clonePlan: " + err.Error())
	}
	var out Plan
	if err := json.Unmarshal(raw, &out); err != nil {
		panic("clonePlan: " + err.Error())
	}
	return &out
}

func (s *fakePlanStore) Create(_ context.Context, p *Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	p.ID = fmt.Sprintf("plan-%d", s.nextID)
	p.Version = 1
	p.CreatedAt = time.Now()
	p.UpdatedAt = p.CreatedAt
	s.plans[p.ID] = clonePlan(p)
	s.created = append(s.created, p.ID)
	return nil
}

func (s *fakePlanStore) Update(_ context.Context, p *Plan) error {
	if hook := s.takeHook(); hook != nil {
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.plans[p.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != p.Version {
		s.conflicts++
		return ErrVersionConflict
	}
	stored := clonePlan(p)
	stored.Version = cur.Version + 1
	stored.UpdatedAt = time.Now()
	s.plans[p.ID] = stored
	p.Version = stored.Version
	s.updates++
	return nil
}

func (s *fakePlanStore) takeHook() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	hook := s.beforeUpdate
	s.beforeUpdate = nil
	return hook
}

// WithDateLock models the Postgres advisory lock the repository takes: one
// holder per date, and a CONTENDER IS REFUSED rather than queued.
//
// Refusing rather than queueing is the point, and is why this double is written
// by hand instead of wrapping a sync.Mutex. A mutex would make every contended
// trial serialize and pass, which would prove the harness cannot tell a lock
// from a queue — and the production lock is pg_try_advisory_xact_lock, whose
// whole contract is "somebody else has it, you get nothing". A test store that
// blocked would be a test store that agrees with any implementation.
//
// grants and refusals are counted so a concurrency trial can prove it actually
// REACHED contention. A 400-trial run that records zero refusals has serialized
// itself by luck and asserted nothing.
func (s *fakePlanStore) WithDateLock(_ context.Context, date string, fn func(context.Context) error) error {
	s.mu.Lock()
	if s.dateLocks == nil {
		s.dateLocks = map[string]bool{}
	}
	if s.dateLocks[date] {
		s.lockRefusals++
		s.mu.Unlock()
		return ErrDateBusy
	}
	s.dateLocks[date] = true
	s.lockGrants++
	s.mu.Unlock()

	// fn is run OUTSIDE s.mu: it calls Get and Update, which take it.
	defer func() {
		s.mu.Lock()
		delete(s.dateLocks, date)
		s.mu.Unlock()
	}()
	return fn(context.Background())
}

// lockCounts reports (grants, refusals) — test-only accessor.
func (s *fakePlanStore) lockCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockGrants, s.lockRefusals
}

func (s *fakePlanStore) Get(_ context.Context, id string) (*Plan, error) {
	s.mu.Lock()
	p, ok := s.plans[id]
	hook := s.afterGet
	s.afterGet = nil
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	out := clonePlan(p)
	// Called outside s.mu, like every other hook here, so a hook that reads or
	// writes the store cannot deadlock on the lock its caller already holds.
	if hook != nil {
		hook()
	}
	return out, nil
}

func (s *fakePlanStore) GetLatestForDate(_ context.Context, date string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.created) - 1; i >= 0; i-- {
		if p, ok := s.plans[s.created[i]]; ok && p.PlanDate == date {
			return clonePlan(p), nil
		}
	}
	return nil, ErrNotFound
}

// ListForDate returns every plan for a date, newest first — the same order the
// repository's `ORDER BY created_at DESC` gives, taken from insertion order
// because two plans minted in the same test tick share a timestamp.
func (s *fakePlanStore) ListForDate(_ context.Context, date string) ([]*Plan, error) {
	s.mu.Lock()
	out := []*Plan{}
	for i := len(s.created) - 1; i >= 0; i-- {
		if p, ok := s.plans[s.created[i]]; ok && p.PlanDate == date {
			out = append(out, clonePlan(p))
		}
	}
	s.listCalls++
	n, hook := s.listCalls, s.afterListForDate
	s.mu.Unlock()
	// Called outside s.mu, like every other hook here, so a hook that reads or
	// writes the store cannot deadlock on the lock its caller already holds.
	if hook != nil {
		hook(n)
	}
	return out, nil
}

// lists reports how many ListForDate calls have been served (test-only).
func (s *fakePlanStore) lists() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
}

// count reports how many plans are stored (test-only accessor). "Did the
// refused ingest create one anyway?" is not answerable without it.
func (s *fakePlanStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.plans)
}

// stored returns the currently persisted plan (test-only accessor).
func (s *fakePlanStore) stored(id string) *Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clonePlan(s.plans[id])
}

// errPlanStore fails every write with a fixed error (for handler mapping).
type errPlanStore struct {
	inner *fakePlanStore
	err   error
}

func (s errPlanStore) Create(ctx context.Context, p *Plan) error { return s.inner.Create(ctx, p) }
func (s errPlanStore) Update(context.Context, *Plan) error       { return s.err }
func (s errPlanStore) Get(ctx context.Context, id string) (*Plan, error) {
	return s.inner.Get(ctx, id)
}
func (s errPlanStore) GetLatestForDate(ctx context.Context, d string) (*Plan, error) {
	return s.inner.GetLatestForDate(ctx, d)
}
func (s errPlanStore) ListForDate(ctx context.Context, d string) ([]*Plan, error) {
	return s.inner.ListForDate(ctx, d)
}
func (s errPlanStore) WithDateLock(ctx context.Context, d string, fn func(context.Context) error) error {
	return s.inner.WithDateLock(ctx, d, fn)
}

// fakeGable is the GableLBM integration double. pushed records every route
// written back so a test can assert what actually reached the dispatch board.
type fakeGable struct {
	// mu makes the double safe to drive from two request goroutines at once.
	// The real ERP is a server; a double that data-races under -race reports
	// its own defect instead of the one under test, and a concurrency trial
	// against it proves nothing. Every method below takes it, and every hook is
	// TAKEN under it and CALLED outside it, so a hook that pushes cannot
	// deadlock on the lock its caller already holds.
	mu sync.Mutex

	orders    []gable.Order
	vehicles  []gable.Vehicle
	drivers   []gable.Driver
	locations []gable.Location

	// locErr simulates a GableLBM that cannot answer the branch lookup —
	// including one that predates /api/integration/locations entirely.
	locErr error

	// locationCalls counts branch lookups. The lookup is a round-trip to the
	// ERP on every ingest, so "was it even asked?" is a behaviour worth
	// asserting, not just an implementation detail.
	locationCalls int

	// orderDates records every date an ingest actually pulled orders for, in
	// call order. It exists because "the supersede gate sits BEFORE the ERP is
	// touched" was unpinnable while this method recorded nothing: moving the
	// gate below the order pull left the whole suite green, and a refused
	// re-ingest would silently keep costing a day of orders and a catalog.
	orderDates []string

	// onListOrders fires once, inside the order pull. That call is exactly the
	// window between the ingest's cheap pre-ERP gate and its authoritative
	// pre-write one, so a test can land a competing push in it.
	onListOrders func()

	pushed  []gable.DeliveryRoute
	pushErr error

	// onFirstPush fires once, INSIDE the first PushDeliveryRoute of a run —
	// the instant this service has written to the dealer's board and not yet
	// written the ledger naming it. It is TAKEN under f.mu and CALLED outside
	// it, so a hook that itself pushes cannot deadlock.
	onFirstPush func()

	// pushErrAfter, when > 0, lets the Nth push and every one before it
	// succeed and fails the rest — the mid-loop failure a partial push is.
	pushErrAfter int
	// pushCalls counts attempts, which is what pushErrAfter counts down. It is
	// not len(pushed): the board below REPLACES rather than accumulates, so the
	// two stopped being the same number.
	pushCalls int

	// recalled records every route withdrawal, in order, so a test can assert
	// that an override recalled EXACTLY the trucks the re-assignment dropped
	// and no others. Recalling a surviving truck would cancel a good route,
	// which is the regression this record exists to catch.
	recalled []gable.RouteRecall
	// recallErr fails every recall (a GableLBM that cannot be reached);
	// recallDispatched fails them with the terminal 409 instead.
	recallErr        error
	recallDispatched bool
	// recallMissing marks the recall as having found nothing on the board —
	// the idempotent no-op, which is a success.
	recallMissing bool
}

func (f *fakeGable) ListOrdersForDate(_ context.Context, date string) ([]gable.Order, error) {
	f.mu.Lock()
	f.orderDates = append(f.orderDates, date)
	hook := f.onListOrders
	f.onListOrders = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orders, nil
}
func (f *fakeGable) ListVehicles(context.Context) ([]gable.Vehicle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vehicles, nil
}
func (f *fakeGable) ListLocations(context.Context) ([]gable.Location, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locationCalls++
	if f.locErr != nil {
		return nil, f.locErr
	}
	return f.locations, nil
}
func (f *fakeGable) ListDrivers(context.Context) ([]gable.Driver, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drivers, nil
}

// PushDeliveryRoute models the upstream write as it actually behaves.
//
// GableLBM's ReplaceDeliveryRoute DELETEs any DRAFT/SCHEDULED delivery_route
// for the same (vehicle_id, scheduled_date) before inserting, so the ERP holds
// AT MOST ONE non-dispatched route per truck per day. A fake that APPENDED
// could represent a board the real one cannot — two plans both holding a truck
// — and every assertion of the form "the ledger mirrors the board" was
// therefore being made against a board that does not exist.
func (f *fakeGable) PushDeliveryRoute(_ context.Context, r gable.DeliveryRoute) error {
	f.mu.Lock()
	hook := f.onFirstPush
	f.onFirstPush = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushCalls++
	if f.pushErrAfter > 0 && f.pushCalls > f.pushErrAfter {
		return fmt.Errorf("gable POST /api/integration/delivery-routes: status 503: upstream unavailable")
	}
	if f.pushErr != nil {
		return f.pushErr
	}
	kept := make([]gable.DeliveryRoute, 0, len(f.pushed)+1)
	for _, existing := range f.pushed {
		if existing.VehicleID == r.VehicleID && existing.ScheduledDate == r.ScheduledDate {
			continue
		}
		kept = append(kept, existing)
	}
	f.pushed = append(kept, r)
	return nil
}

func (f *fakeGable) RecallDeliveryRoute(_ context.Context, rc gable.RouteRecall) (*gable.RouteRecallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recallDispatched {
		return nil, fmt.Errorf("%w (truck %s on %s)", gable.ErrRouteDispatched, rc.VehicleID, rc.ScheduledDate)
	}
	if f.recallErr != nil {
		return nil, f.recallErr
	}
	f.recalled = append(f.recalled, rc)
	if f.recallMissing {
		return &gable.RouteRecallResult{Recalled: false}, nil
	}
	// Drop the recalled route from the board the fake is standing in for, so
	// "what is live upstream" stays honest across a push/recall/push cycle.
	kept := f.pushed[:0]
	for _, r := range f.pushed {
		if r.VehicleID != rc.VehicleID || r.ScheduledDate != rc.ScheduledDate {
			kept = append(kept, r)
		}
	}
	stops := 0
	if n := len(f.pushed) - len(kept); n > 0 {
		stops = 1
	}
	f.pushed = kept
	return &gable.RouteRecallResult{Recalled: stops > 0, RouteID: "route-" + rc.VehicleID, StopCount: stops}, nil
}

// recalledIDs lists the vehicles recalled, in call order.
func (f *fakeGable) recalledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.recalled))
	for _, r := range f.recalled {
		out = append(out, r.VehicleID)
	}
	return out
}

// pushedIDs lists the vehicles currently live on the fake dispatch board.
func (f *fakeGable) pushedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.pushed))
	for _, r := range f.pushed {
		out = append(out, r.VehicleID)
	}
	return out
}

// fakeCatalog resolves products to effective geometry.
type fakeCatalog struct{ products []catalog.EffectiveProduct }

func (f *fakeCatalog) ListEffectiveProducts(context.Context) ([]catalog.EffectiveProduct, error) {
	return f.products, nil
}

// fakeFleet serves stored vehicle profiles (ErrNotFound ⇒ auto-provision).
type fakeFleet struct{ profiles map[string]*fleet.Profile }

func (f *fakeFleet) GetProfile(_ context.Context, id string) (*fleet.Profile, error) {
	if p, ok := f.profiles[id]; ok {
		return p, nil
	}
	return nil, fleet.ErrNotFound
}

func (f *fakeFleet) UpsertProfile(_ context.Context, id string, in fleet.ProfileInput) (*fleet.Profile, error) {
	if f.profiles == nil {
		f.profiles = map[string]*fleet.Profile{}
	}
	p := &fleet.Profile{
		GableVehicleID: id,
		Name:           in.Name,
		BedLengthIn:    in.BedLengthIn,
		BedWidthIn:     in.BedWidthIn,
		BedHeightIn:    in.BedHeightIn,
		GVWRLbs:        in.GVWRLbs,
		TareWeightLbs:  in.TareWeightLbs,
	}
	for _, a := range in.Axles {
		p.Axles = append(p.Axles, fleet.Axle{
			AxleNumber:          a.AxleNumber,
			MaxWeightLbs:        a.MaxWeightLbs,
			PositionFromFrontIn: a.PositionFromFrontIn,
			AxleType:            a.AxleType,
		})
	}
	f.profiles[id] = p
	return p, nil
}

// fakeChecker returns a canned restricted-point verdict.
type fakeChecker struct {
	result *compliance.RouteCheckResult
	calls  int
}

func (f *fakeChecker) CheckRoute(context.Context, compliance.RouteCheckRequest) (*compliance.RouteCheckResult, error) {
	f.calls++
	if f.result != nil {
		return f.result, nil
	}
	return &compliance.RouteCheckResult{Status: "PASS", Flags: []compliance.Flag{}}, nil
}

// fakeBriefer is an unconfigured AI client (the workflow must never need it).
type fakeBriefer struct{}

func (fakeBriefer) Configured() bool { return false }
func (fakeBriefer) Model() string    { return "" }
func (fakeBriefer) Generate(context.Context, string, string, int) (string, error) {
	return "", fmt.Errorf("not configured")
}

// newTestService wires a Service over the supplied store with inert doubles.
func newTestService(store planStore, g *fakeGable, cfg Config) *Service {
	if g == nil {
		g = &fakeGable{}
	}
	return NewService(store, g, &fakeCatalog{}, &fakeFleet{}, &fakeChecker{}, fakeBriefer{}, cfg)
}

func fptr(f float64) *float64 { return &f }
