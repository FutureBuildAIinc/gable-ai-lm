// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// holdsDate reports whether this date is claimed RIGHT NOW — test-only.
//
// It exists because the only other way to ask is to send a competing writer,
// and a writer that is ALLOWED through does not just answer the question, it
// changes the board mid-flight and the reconciliation that follows then
// describes a state the test created rather than the one under test. Asking the
// lock directly answers "is the claim held here?" without touching anything.
func (s *fakePlanStore) holdsDate(date string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dateLocks[date]
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

	// pushed is the dispatch board as this double holds it: one boardRow per
	// delivery_routes row, NOT one per truck. Two rows for one truck on one day
	// is a state the real ERP produces (migration 009 declines a unique index on
	// (vehicle_id, scheduled_date); the dealer's own CreateRoute inserts with no
	// dedup), and a double that could not represent it could not fail the way
	// production did.
	pushed  []boardRow
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

	// --- the dispatch board, as a READ ---
	//
	// The board this double reports is DERIVED from f.pushed, never tracked
	// beside it. That is the whole point: the fake ERP must not be able to hold
	// a board that disagrees with itself, or a test could "prove" the
	// reconciler right against a board that does not exist. A route reaches
	// this board by being pushed (by the service, or by pushOutside standing in
	// for a dispatcher working directly in GableLBM) and leaves it by being
	// recalled.

	// boardStatus overrides a truck's status on the board. Absent means
	// SCHEDULED, which is what GableLBM's ReplaceDeliveryRoute actually writes.
	// It is how a test makes a truck IN_TRANSIT — a route that is real, on the
	// board, and can never be recalled — or gives it a status this service does
	// not recognise at all, which delivery_routes.status (VARCHAR(50), no CHECK
	// constraint) genuinely permits.
	boardStatus map[string]string

	// nextRouteID numbers the rows this double mints, and it only ever goes up.
	//
	// The board used to derive a route id from a row's INDEX in f.pushed, which
	// meant an id changed identity whenever an earlier row was recalled. An
	// identity that moves is not an identity, and the code under test now
	// matches ledger claims to board rows by exactly this value — a fake that
	// recycled ids could make a wrong implementation look right.
	nextRouteID int

	// boardErr makes the board unreadable (an unreachable or broken ERP).
	boardErr error

	// boardCalls counts board reads. "Was the board consulted at all, and how
	// many times?" is the difference between a gate that reads the system of
	// record and one that still trusts its cache, and no end-state assertion
	// can tell them apart.
	boardCalls int

	// onBoardRead fires INSIDE every board read, with that read's 1-based
	// count. It is the only seam that can answer "was this read taken under the
	// dispatch-date claim?": a hook that tries to write the same date and is
	// refused proves the claim was held, which no end-state assertion can,
	// because every ordering converges when nothing lands in between.
	//
	// It takes the count because a re-plan reads the board TWICE — once
	// outside the claim to make a refusal honest before three heavier ERP
	// pulls, once inside it to decide — and the two reads must be told apart:
	// a hook that could only see the first would report the exact opposite of
	// the truth about where the claim is taken.
	onBoardRead func(n int)
}

// boardRow is one row of the fake dispatch board: the route as it was written,
// plus the identity the ERP minted for it. The id is stored, never derived,
// because it is the thing the code under test matches on.
type boardRow struct {
	route   gable.DeliveryRoute
	routeID string
}

// ListDeliveryRoutesForDate reports the board the pushes have actually built.
func (f *fakeGable) ListDeliveryRoutesForDate(_ context.Context, date string) ([]gable.BoardRoute, error) {
	f.mu.Lock()
	f.boardCalls++
	n, hook := f.boardCalls, f.onBoardRead
	f.mu.Unlock()
	// Called outside f.mu, like every other hook here, so a hook that itself
	// drives the ERP double cannot deadlock on the lock its caller holds.
	if hook != nil {
		hook(n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.boardErr != nil {
		return nil, f.boardErr
	}
	out := []gable.BoardRoute{}
	for _, row := range f.pushed {
		r := row.route
		if r.ScheduledDate != date {
			continue
		}
		status := gable.RouteStatusScheduled
		if st, ok := f.boardStatus[r.VehicleID]; ok {
			status = st
		}
		orderIDs := make([]string, 0, len(r.Stops))
		for _, st := range r.Stops {
			orderIDs = append(orderIDs, st.OrderID)
		}
		out = append(out, gable.BoardRoute{
			RouteID:       row.routeID,
			VehicleID:     r.VehicleID,
			DriverID:      r.DriverID,
			Status:        status,
			ScheduledDate: r.ScheduledDate,
			StopCount:     len(r.Stops),
			OrderIDs:      orderIDs,
		})
	}
	return out, nil
}

// mintRouteID hands out the next never-reused delivery_routes.id. Callers hold
// f.mu.
//
// The value is deterministic but deliberately NOT ordered by insertion, because
// the real column is `id UUID PRIMARY KEY DEFAULT uuid_generate_v4()` and a v4
// UUID carries no order information whatever. Ids that ascended with insertion
// would make a reconciler that merely preserved the ERP's scan order look as if
// it sorted by route id, which is the one thing the report's determinism now
// rests on.
func (f *fakeGable) mintRouteID(vehicleID string) string {
	f.nextRouteID++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", vehicleID, f.nextRouteID)))
	return "dr-" + hex.EncodeToString(sum[:6])
}

// boardReads reports how many times the board was consulted (test-only).
func (f *fakeGable) boardReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boardCalls
}

// pushOutside puts a route on the board that this service did NOT write — a
// dispatcher creating a run by hand in GableLBM, or the surviving half of a
// push whose ledger write was lost to a crash.
//
// It writes to the SAME f.pushed the service's own pushes land in, rather than
// to a side list, so the acceptance oracle and the reconciler see one board.
// An orphan that only the code under test could see would prove nothing.
func (f *fakeGable) pushOutside(vehicleID, date, status string, orderIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stops := make([]gable.RouteStop, 0, len(orderIDs))
	for i, id := range orderIDs {
		stops = append(stops, gable.RouteStop{OrderID: id, Sequence: i + 1})
	}
	// APPENDED, never replacing. This stands in for a row the SERVICE did not
	// write — most importantly internal/delivery/repository.go's CreateRoute,
	// the dealer's own dispatch UI, which inserts with no dedup of any kind. A
	// truck that already carries our route can therefore end up with a second,
	// hand-built one, which is the state that used to be invisible.
	f.pushed = append(f.pushed, boardRow{
		route:   gable.DeliveryRoute{VehicleID: vehicleID, ScheduledDate: date, Stops: stops},
		routeID: f.mintRouteID(vehicleID),
	})
	if status != "" && status != gable.RouteStatusScheduled {
		if f.boardStatus == nil {
			f.boardStatus = map[string]string{}
		}
		f.boardStatus[vehicleID] = status
	}
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
func (f *fakeGable) PushDeliveryRoute(_ context.Context, r gable.DeliveryRoute) (*gable.RouteAck, error) {
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
		return nil, fmt.Errorf("gable POST /api/integration/delivery-routes: status 503: upstream unavailable")
	}
	if f.pushErr != nil {
		return nil, f.pushErr
	}
	kept := make([]boardRow, 0, len(f.pushed)+1)
	replaced := false
	for _, existing := range f.pushed {
		if existing.route.VehicleID == r.VehicleID && existing.route.ScheduledDate == r.ScheduledDate {
			// The upstream DELETE only reaches DRAFT/SCHEDULED rows, and it
			// reaches ALL of them — including one a dispatcher built by hand.
			// That is why a push over a contested truck destroys the other run
			// and why nothing in this package may reach a push in that state
			// without a human having looked.
			if st, ok := f.boardStatus[existing.route.VehicleID]; ok &&
				(st == gable.RouteStatusInTransit || st == gable.RouteStatusCompleted) {
				kept = append(kept, existing)
				continue
			}
			replaced = true
			continue
		}
		kept = append(kept, existing)
	}
	// A FRESH id, exactly as ReplaceDeliveryRoute does: it DELETEs the prior row
	// and INSERTs a new one, so the id a re-push lands on is never the id the
	// previous push recorded.
	row := boardRow{route: r, routeID: f.mintRouteID(r.VehicleID)}
	f.pushed = append(kept, row)
	return &gable.RouteAck{RouteID: row.routeID, StopCount: len(r.Stops), Created: true, Replaced: replaced}, nil
}

func (f *fakeGable) RecallDeliveryRoute(_ context.Context, rc gable.RouteRecall) (*gable.RouteRecallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A truck that has left the yard cannot be recalled, and GableLBM says so
	// with a 409. Modelling it from the SAME boardStatus the board read is
	// derived from is what makes "IN_TRANSIT is never treated as reclaimable"
	// an assertion about behaviour rather than about a flag: a reconciler that
	// wrongly classified a departed truck as reclaimable would reach this and
	// fail, instead of quietly succeeding against a permissive double.
	if st, ok := f.boardStatus[rc.VehicleID]; ok &&
		(st == gable.RouteStatusInTransit || st == gable.RouteStatusCompleted) {
		return nil, fmt.Errorf("%w (truck %s on %s)", gable.ErrRouteDispatched, rc.VehicleID, rc.ScheduledDate)
	}
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
	// Keyed (vehicle_id, scheduled_date) and nothing finer, because that is the
	// only key the endpoint accepts. Every row for the pair goes — which is
	// exactly why a second, hand-built row for a truck we also hold cannot be
	// withdrawn on its own, and why boardTruth refuses instead of offering an
	// approval it could not honour.
	kept := f.pushed[:0]
	for _, r := range f.pushed {
		if r.route.VehicleID != rc.VehicleID || r.route.ScheduledDate != rc.ScheduledDate {
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
		out = append(out, r.route.VehicleID)
	}
	return out
}

// removeFromBoard takes a route off the board WITHOUT telling this service — a
// dispatcher cancelling a run in GableLBM's own UI, or the ERP side of a
// half-applied change. It is how a GHOST is made: our ledger goes on claiming a
// truck the system of record no longer holds.
func (f *fakeGable) removeFromBoard(vehicleID, date string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := make([]boardRow, 0, len(f.pushed))
	for _, r := range f.pushed {
		if r.route.VehicleID == vehicleID && r.route.ScheduledDate == date {
			continue
		}
		kept = append(kept, r)
	}
	f.pushed = kept
	delete(f.boardStatus, vehicleID)
}

// cancelRow takes ONE row off the board by its route id — a dispatcher opening
// GableLBM and cancelling the run they built by hand, leaving ours alone.
//
// removeFromBoard cannot express that: it is keyed (vehicle, date), like the
// recall wire call, and clears every row for the truck. The difference is the
// whole reason a contested orphan is refused rather than approved — this double
// must be able to show the remedy actually working.
func (f *fakeGable) cancelRow(routeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := make([]boardRow, 0, len(f.pushed))
	for _, r := range f.pushed {
		if r.routeID == routeID {
			continue
		}
		kept = append(kept, r)
	}
	f.pushed = kept
}

// unassignVehicle nulls a row's vehicle without touching anything else —
// delivery_routes.vehicle_id is nullable upstream and nothing stops a route
// AI_LM wrote from losing its truck. The recall key is (vehicle_id,
// scheduled_date), so the row becomes un-nameable on the wire while our ledger
// still holds a claim on it.
func (f *fakeGable) unassignVehicle(routeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.pushed {
		if f.pushed[i].routeID == routeID {
			f.pushed[i].route.VehicleID = ""
		}
	}
}

// setBoardStatus moves a truck's route to another status on the board — most
// importantly IN_TRANSIT, which is a route that is real, still on the board,
// and can never be recalled.
func (f *fakeGable) setBoardStatus(vehicleID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.boardStatus == nil {
		f.boardStatus = map[string]string{}
	}
	f.boardStatus[vehicleID] = status
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
