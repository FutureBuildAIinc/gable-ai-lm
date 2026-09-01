// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file states, and pins, the two properties every live-route gate in this
// package silently depends on. Both are about a DATE, not a plan, because a
// date legitimately holds several plans — that is what "supersede rather than
// replace" means — and nothing makes the plan holding live routes the newest.
//
//	I.  No reachable sequence of HTTP calls leaves two plans for one date
//	    simultaneously claiming the SAME truck live.
//	II. No live route exists upstream that no plan's ledger names, and no
//	    ledger names a live route upstream no longer has. The ledger is a
//	    MIRROR; if it can silently diverge, every gate keyed on it — including
//	    the supersede gate — is unsound.
//
// I is not a tidiness rule. GableLBM's ReplaceDeliveryRoute DELETEs any
// DRAFT/SCHEDULED delivery_route for the same (vehicle_id, scheduled_date)
// before inserting, so the ERP holds AT MOST ONE non-dispatched route per truck
// per day: two plans both claiming a truck is a state the dealer's system
// cannot represent, and one of the two ledgers has been lying since the second
// push destroyed the first plan's route.
//
// Every test here drives the real handlers. The defect that prompted them was
// CONFIRMED against a gate that reads correctly — the gate worked exactly as
// described and the harm was still reachable in three plain HTTP calls, with no
// concurrency, because the gate was fed GetLatestForDate: exactly ONE plan.

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// dispatchDay is a date driven exclusively through the HTTP surface, so nothing
// asserted below depends on a service call a real client could not make.
type dispatchDay struct {
	t     *testing.T
	mux   *http.ServeMux
	svc   *Service
	store *fakePlanStore
	g     *fakeGable
	date  string
}

func newDispatchDay(t *testing.T, plans ...*Plan) *dispatchDay {
	t.Helper()
	ids := map[string]bool{}
	for _, p := range plans {
		for _, l := range p.Loads {
			ids[l.VehicleID] = true
		}
	}
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)

	store := newFakePlanStore(plans...)
	g := fleetOf(names...)
	svc := newTestService(store, g, Config{})
	mux := http.NewServeMux()
	NewHandler(svc).RegisterRoutes(mux)
	return &dispatchDay{t: t, mux: mux, svc: svc, store: store, g: g, date: "2026-06-26"}
}

func (d *dispatchDay) post(path, body string) *httptest.ResponseRecorder {
	d.t.Helper()
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// ingest re-plans the day over the wire.
func (d *dispatchDay) ingest(body string) *httptest.ResponseRecorder {
	d.t.Helper()
	return d.post("/api/v1/workflow/plans", body)
}

// push sends one plan to the dispatch board over the wire and fails the test if
// it does not land, so the assertions start from a known-live board.
func (d *dispatchDay) push(planID string) {
	d.t.Helper()
	if rec := d.post("/api/v1/workflow/plans/"+planID+"/push", ``); rec.Code != http.StatusOK {
		d.t.Fatalf("setup: push %s answered %d (%s)", planID, rec.Code, rec.Body.String())
	}
}

// claims maps every truck some plan for this date still HOLDS — published as
// intent or confirmed on the board — to the plans holding it.
func (d *dispatchDay) claims() map[string][]string {
	d.t.Helper()
	return d.claimsWhere(func(LiveRoute) bool { return true })
}

// confirmedClaims is claims narrowed to the trucks a plan says are KNOWN to be
// on the dispatch board.
func (d *dispatchDay) confirmedClaims() map[string][]string {
	d.t.Helper()
	return d.claimsWhere(LiveRoute.Confirmed)
}

func (d *dispatchDay) claimsWhere(keep func(LiveRoute) bool) map[string][]string {
	d.t.Helper()
	plans, err := d.store.ListForDate(context.Background(), d.date)
	if err != nil {
		d.t.Fatalf("list plans: %v", err)
	}
	out := map[string][]string{}
	for _, p := range plans {
		for _, r := range liveRoutes(p) {
			if keep(r) {
				out[r.VehicleID] = append(out[r.VehicleID], p.ID)
			}
		}
	}
	return out
}

// board lists the trucks actually holding a route upstream for this date.
func (d *dispatchDay) board() map[string]bool {
	out := map[string]bool{}
	for _, r := range d.g.pushed {
		if r.ScheduledDate == d.date {
			out[r.VehicleID] = true
		}
	}
	return out
}

// assertAcceptance is the acceptance test itself, run against whatever state the
// handlers have actually reached. Both halves are asserted from OBSERVED state:
// the ledgers as stored, and the board as the ERP double holds it.
func (d *dispatchDay) assertAcceptance() {
	d.t.Helper()
	for _, v := range d.acceptanceViolations() {
		d.t.Error(v)
	}
}

// acceptanceViolations is assertAcceptance's verdict as DATA rather than as a
// test failure, so a concurrency trial can run the same oracle several hundred
// times and report how many trials violated it. Reporting "no failures
// observed" without a trial count is how a 1-in-400 defect gets called closed;
// the count is the deliverable, so the oracle has to be countable.
// assertAcceptanceExceptStranded is assertAcceptance for the one state this
// system cannot get itself out of: a route GableLBM ITSELF refused to take back.
//
// It is deliberately not a way to skip the oracle. Acceptance I is asserted in
// full, acceptance II is asserted in full for every truck but the named ones,
// and the named ones have to be named — a test that has to reach for this is
// stating, on the record, which truck the dealer is left dispatching and why.
func (d *dispatchDay) assertAcceptanceExceptStranded(reason string, vehicles ...string) {
	d.t.Helper()
	if reason == "" {
		d.t.Fatal("assertAcceptanceExceptStranded needs a reason: an unnamed exception is a skipped oracle")
	}
	for _, v := range d.acceptanceViolations(vehicles...) {
		d.t.Error(v)
	}
}

func (d *dispatchDay) acceptanceViolations(stranded ...string) []string {
	d.t.Helper()
	claims, confirmed, board := d.claims(), d.confirmedClaims(), d.board()
	unrecallable := map[string]bool{}
	for _, v := range stranded {
		unrecallable[v] = true
	}
	var out []string

	for vehicle, planIDs := range claims {
		if len(planIDs) > 1 {
			out = append(out, fmt.Sprintf("acceptance I: truck %s on %s is claimed live by %v — the ERP holds at most one non-dispatched route per truck per day, so at least one of those ledgers is lying",
				vehicle, d.date, sorted(planIDs)))
		}
	}
	for vehicle := range board {
		if len(claims[vehicle]) == 0 && !unrecallable[vehicle] {
			out = append(out, fmt.Sprintf("acceptance II: truck %s is live on the dispatch board for %s and NO plan\u2019s ledger names it — nothing in this system can ever recall it",
				vehicle, d.date))
		}
	}
	// The mirror clause is asserted on CONFIRMED claims, and deliberately not
	// on published ones.
	//
	// "Claimed but not on the board" is not a divergence to be stamped out — it
	// is the window this design chose, spelled out. Push publishes its claim
	// before the wire call so the gap fails as an OVER-CLAIM rather than as an
	// orphan, and asserting that every claim matches the board would be
	// asserting that window away and taking the orphan back. The over-claim
	// costs a redundant, idempotent recall (recallRoutes says so itself) and is
	// bounded by the claim lease. A CONFIRMED claim makes the stronger promise —
	// "this route is up there" — and every gate and recall path in this package
	// believes it, so that one is held to the board exactly.
	for vehicle, planIDs := range confirmed {
		if !board[vehicle] {
			out = append(out, fmt.Sprintf("acceptance II: %v claim truck %s CONFIRMED live on %s but the board holds no such route — every gate keyed on that ledger is reading a lie",
				sorted(planIDs), vehicle, d.date))
		}
	}
	sort.Strings(out)
	return out
}

// planForTrucks builds a REVIEWED, packed, signed plan carrying exactly the
// named trucks, so a date can be given two plans that hold DIFFERENT trucks (or
// deliberately overlapping ones). The ids are left unset so the store numbers
// them in seed order.
func planForTrucks(vehicleIDs ...string) *Plan {
	p := multiTruckPushReadyPlan(len(vehicleIDs))
	p.ID = ""
	for i, id := range vehicleIDs {
		orderID := "o-" + id
		p.Loads[i].VehicleID = id
		p.Loads[i].VehicleName = "Flatbed " + strings.TrimPrefix(id, "v")
		p.Loads[i].Stops[0].OrderID = orderID
		p.Orders[i].OrderID = orderID
	}
	return p
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Finding A: the gate was fed exactly one plan
// ---------------------------------------------------------------------------

// TestReplanningADateAnOlderPlanStillHoldsIsRefused is the observed defect,
// stated as three plain POSTs.
//
// Against the previous gate this sequence answered 201: the gate asked
// GetLatestForDate, which named the plan minted by call 1 (empty ledger), and
// never saw that call 2 had put the FIRST plan's two trucks on the board. Zero
// recalls in the whole sequence; plan-1 left holding two live routes and
// unreachable from its own date. No race is needed — two dispatchers both
// re-planning today, one pushing the plan they had open, is the whole story.
func TestReplanningADateAnOlderPlanStillHoldsIsRefused(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the first re-plan is free (nothing is live yet): %d %s", rec.Code, rec.Body.String())
	}
	d.push("plan-1") // the OLDER plan goes to the board, by id
	if got := d.g.pushedIDs(); len(got) != 2 {
		t.Fatalf("setup: the board holds %v, want both trucks", got)
	}
	planned := d.store.count()
	writes := d.store.updates
	pulls := len(d.g.orderDates)

	rec := d.ingest(`{"date":"2026-06-26"}`)

	if rec.Code != http.StatusLocked {
		t.Fatalf("re-planning a date an OLDER plan still holds answered %d, want 423 — %s",
			rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"manual approval", "Flatbed 1", "Flatbed 2", "plan-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal must name %q so the dispatcher knows what they are stranding: %s", want, body)
		}
	}
	// A refusal that created the plan anyway is the defect with a message on it.
	if n := d.store.count(); n != planned {
		t.Errorf("the store holds %d plans, want %d — a refused re-plan creates nothing", n, planned)
	}
	if d.store.updates != writes {
		t.Errorf("a refused re-plan wrote %d time(s); it must write nothing", d.store.updates-writes)
	}
	if len(d.g.recalled) != 0 {
		t.Errorf("a refused re-plan must not touch the board, recalled %v", d.g.recalledIDs())
	}
	if got := sorted(d.g.pushedIDs()); len(got) != 2 {
		t.Errorf("the board must be left exactly as it was, holds %v", got)
	}
	// The gate sits BEFORE the ERP is touched: a dispatcher who needs an
	// approver is not a reason to pull a day of orders and a catalog and throw
	// them away. Moving the gate below the pull is invisible to every other
	// assertion in this package.
	if n := len(d.g.orderDates) - pulls; n != 0 {
		t.Errorf("the refused re-plan pulled orders %d time(s); the gate must sit before the ERP round-trips", n)
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// the override: every live plan on the date, not the latest one
// ---------------------------------------------------------------------------

// TestAnApprovedReplanRecallsEveryLivePlanOnTheDate pins the other half. Two
// plans hold this date and BOTH have trucks out. A re-plan replaces the date,
// not the newest plan on it, so every route across every one of them has to
// come off the board first — and each ledger has to be tombstoned, or the plan
// this one did not clear stays invisible to the next gate.
func TestAnApprovedReplanRecallsEveryLivePlanOnTheDate(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v3", "v4"))
	d.push("plan-1")
	d.push("plan-2")
	if got := sorted(d.g.pushedIDs()); len(got) != 4 {
		t.Fatalf("setup: the board holds %v, want four trucks", got)
	}
	// Order is a claim in its own right: "recall, THEN create". Two end-state
	// assertions cannot tell that apart from "create, then recall", which leaves
	// a window with a new plan sitting on a board still holding the old routes.
	order := &recallOrderStore{fakePlanStore: d.store, g: d.g}
	d.svc.repo = order

	rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("an approved re-plan must proceed: %d %s", rec.Code, rec.Body.String())
	}

	if got, want := sorted(d.g.recalledIDs()), []string{"v1", "v2", "v3", "v4"}; !equalStrings(got, want) {
		t.Errorf("recalled %v, want every truck live on the date across BOTH plans %v", got, want)
	}
	if got := d.g.pushedIDs(); len(got) != 0 {
		t.Errorf("the dealer's board still holds %v — a re-plan keeps nothing", got)
	}
	if order.recallsAtCreate != 4 {
		t.Errorf("the new plan was created after %d recall(s), want 4 — the board is cleared BEFORE a plan is put on top of it",
			order.recallsAtCreate)
	}
	for _, id := range []string{"plan-1", "plan-2"} {
		old := d.store.stored(id)
		if n := len(liveRoutes(old)); n != 0 {
			t.Errorf("%s still claims %d live route(s) — every superseded ledger is tombstoned, not just the latest plan's", id, n)
		}
		if len(old.LiveRoutes) != 2 {
			t.Errorf("%s: recalls are tombstoned, never removed: %+v", id, old.LiveRoutes)
		}
		for _, r := range old.LiveRoutes {
			if r.RecalledBy != "dispatcher@dealer.com" || r.RecallNote == "" {
				t.Errorf("%s tombstone %+v must record who approved the recall and why", id, r)
			}
		}
		if n := len(old.PushedOverrides); n != 1 {
			t.Errorf("%s carries %d approval(s), want exactly 1 — the override is recorded on every plan it authorized changing, once", id, n)
		} else if ov := old.PushedOverrides[0]; ov.Action != actionSupersede || ov.ApprovedBy != "dispatcher@dealer.com" {
			t.Errorf("%s override %+v must name the action and the approver", id, ov)
		}
	}
	if n := d.store.count(); n != 3 {
		t.Errorf("the store holds %d plans, want 3 (both superseded plans are kept for audit)", n)
	}
	d.assertAcceptance()
}

// TestTheRefusalNamesEveryPlanHoldingTheDate keeps the 423 honest when the date
// is held by more than one plan. It is ONE sentence — the same override idiom
// every other gate in this package writes — listing what an approval would cost
// across all of them, because a dispatcher told about one of two live plans
// approves a withdrawal they have only half understood.
func TestTheRefusalNamesEveryPlanHoldingTheDate(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v3", "v4"))
	d.push("plan-1")
	d.push("plan-2")

	body := d.ingest(`{"date":"2026-06-26"}`).Body.String()
	for _, want := range []string{"plan-1", "plan-2", "Flatbed 1", "Flatbed 2", "Flatbed 3", "Flatbed 4"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal must name %q — it is what the approval would withdraw: %s", want, body)
		}
	}
}

// ---------------------------------------------------------------------------
// requirement 4: a push corrects the ledger it just invalidated
// ---------------------------------------------------------------------------

// TestAPushThatReplacesAnotherPlansTruckClearsThatPlansClaim is the fix for the
// cross-plan recall, closed at the source rather than at the recall.
//
// GableLBM's ReplaceDeliveryRoute DELETEs the DRAFT/SCHEDULED route for the
// same (vehicle_id, scheduled_date) before inserting. So the instant plan-2
// pushes v1, plan-1's route for v1 is GONE and plan-1's ledger is lying. Left
// lying it poisons everything downstream: the recall wire key is (vehicle,
// date), not "the route this plan pushed", so superseding plan-1 would cancel
// plan-2's live route, and the gate would refuse future re-ingests over ghosts.
func TestAPushThatReplacesAnotherPlansTruckClearsThatPlansClaim(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v3"))
	d.push("plan-1")
	if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v1", "v2"}) {
		t.Fatalf("setup: the board holds %v", got)
	}

	d.push("plan-2") // v1's route upstream is now plan-2's; plan-1's is deleted

	old := d.store.stored("plan-1")
	if got := sorted(liveVehicleIDs(old)); !equalStrings(got, []string{"v2"}) {
		t.Errorf("plan-1 claims %v live, want only v2 — its v1 route was destroyed upstream the moment plan-2 pushed the same truck", got)
	}
	var v1 *LiveRoute
	for i := range old.LiveRoutes {
		if old.LiveRoutes[i].VehicleID == "v1" {
			v1 = &old.LiveRoutes[i]
		}
	}
	if v1 == nil || v1.Live() {
		t.Fatalf("plan-1's v1 entry must be tombstoned, got %+v", v1)
	}
	if !strings.Contains(v1.RecallNote, "plan-2") || v1.RecalledBy == "" {
		t.Errorf("tombstone %+v must say what replaced the route and that this system, not a dispatcher, did it", *v1)
	}
	// This is a ledger correction, NOT a recall: there is nothing left upstream
	// to withdraw, so no wire call may be made and no route may be cancelled.
	if len(d.g.recalled) != 0 {
		t.Errorf("a displaced claim is corrected, never recalled — recalled %v, which would cancel plan-2's live route", d.g.recalledIDs())
	}
	if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v1", "v2", "v3"}) {
		t.Errorf("the board holds %v, want v1 (plan-2's), v2 and v3", got)
	}
	d.assertAcceptance()

	// The payoff. Superseding the date now recalls each truck once, from the
	// plan that really holds it, and leaves nothing behind on either ledger.
	if rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("an approved re-plan must proceed: %d %s", rec.Code, rec.Body.String())
	}
	if got, want := sorted(d.g.recalledIDs()), []string{"v1", "v2", "v3"}; !equalStrings(got, want) {
		t.Errorf("recalled %v, want %v — each live truck exactly once, from the plan that actually holds it", got, want)
	}
	if got := d.g.pushedIDs(); len(got) != 0 {
		t.Errorf("the board still holds %v", got)
	}
	d.assertAcceptance()
}

// TestAResumeRestoresARouteAnotherPlanTookOver is the small companion to the
// rule above, and it is why the resume skip consults the ledger and not just
// the digest.
//
// A push that died part-way leaves truck 1 acked and the plan at REVIEWED, so
// re-running it is an ordinary resume. If another plan has taken that truck in
// the meantime, its route upstream is GONE — the digest still says "I sent
// this", but the ledger, which the correction above tombstoned, says it is no
// longer up there. Skipping on the digest alone would leave that truck with no
// route at all and a plan convinced it had dispatched one: the yard loads for a
// run the dealer's board has never heard of.
func TestAResumeRestoresARouteAnotherPlanTookOver(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v3"))

	// plan-1's push dies after truck 1. v1 is acked and live; the plan stays
	// REVIEWED, which is exactly what makes the retry an ordinary push.
	d.g.pushErrAfter = 1
	if rec := d.post("/api/v1/workflow/plans/plan-1/push", ``); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("setup: the partial push must be reported, got %d (%s)", rec.Code, rec.Body.String())
	}
	d.g.pushErrAfter = 0
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v1"}) {
		t.Fatalf("setup: plan-1 claims %v, want just the truck that landed", got)
	}

	// Another plan takes v1. Upstream, plan-1's route for it is deleted.
	d.push("plan-2")
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); len(got) != 0 {
		t.Fatalf("setup: plan-1 still claims %v after losing v1", got)
	}

	sent := d.g.pushCalls
	d.push("plan-1") // the resume

	if wrote := d.g.pushCalls - sent; wrote != 2 {
		t.Errorf("the resume wrote %d route(s), want 2 — v1 has no route upstream any more and must be re-sent, not skipped on a digest that only records what this plan once posted", wrote)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("plan-1 claims %v, want both trucks back on the board", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v3"}) {
		t.Errorf("plan-2 claims %v, want only v3 — it has just lost v1 the same way plan-1 did", got)
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// the TOCTOU
// ---------------------------------------------------------------------------

// TestAPushLandingInsideTheIngestWindowDoesNotOrphan closes the read-before-the-
// ERP hole. The gate's snapshot is taken BEFORE three ERP round-trips (orders,
// catalog, branches) and the previous code never looked again: a push landing
// in that window was planned straight over. Worse, the fast path — nothing
// doomed — returned before repo.Update, so even the optimistic-concurrency
// check that might have caught the stale read never ran.
//
// The hook fires inside the order pull, which is exactly that window.
func TestAPushLandingInsideTheIngestWindowDoesNotOrphan(t *testing.T) {
	t.Run("unapproved: refused, nothing created", func(t *testing.T) {
		d := newDispatchDay(t, planForTrucks("v1", "v2"))
		d.g.onListOrders = func() { d.push("plan-1") }
		planned := d.store.count()

		rec := d.ingest(`{"date":"2026-06-26"}`)

		if rec.Code != http.StatusLocked {
			t.Fatalf("a push that landed mid-ingest must be seen: answered %d (%s)", rec.Code, rec.Body.String())
		}
		if n := d.store.count(); n != planned {
			t.Errorf("the store holds %d plans, want %d — a refused re-plan creates nothing", n, planned)
		}
		if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v1", "v2"}) {
			t.Errorf("the board holds %v, want the routes that just landed, untouched", got)
		}
		d.assertAcceptance()
	})

	t.Run("approved: the late arrival is recalled too", func(t *testing.T) {
		d := newDispatchDay(t, planForTrucks("v1", "v2"))
		d.g.onListOrders = func() { d.push("plan-1") }

		rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)

		if rec.Code != http.StatusCreated {
			t.Fatalf("an approved re-plan must proceed: %d %s", rec.Code, rec.Body.String())
		}
		if got, want := sorted(d.g.recalledIDs()), []string{"v1", "v2"}; !equalStrings(got, want) {
			t.Errorf("recalled %v, want %v — the routes that landed inside the window must come off too", got, want)
		}
		if got := d.g.pushedIDs(); len(got) != 0 {
			t.Errorf("the board still holds %v", got)
		}
		d.assertAcceptance()
	})
}

// staleSnapshotStore answers every by-date read with one fixed snapshot, so the
// ingest carries a plan object the stored row has already moved past. Re-reading
// closes the wide TOCTOU window (the three ERP round-trips); this store models
// what is left of it, and the version check on the write is what must catch it.
type staleSnapshotStore struct {
	*fakePlanStore
	stale *Plan
}

func (s *staleSnapshotStore) ListForDate(context.Context, string) ([]*Plan, error) {
	return []*Plan{clonePlan(s.stale)}, nil
}

// TestASupersedeOnAStaleSnapshotConflictsRatherThanProceeds pins that the write
// is what catches a snapshot somebody else has already moved past. Without it
// the ingest would recall on the strength of stale state and then create a plan
// over a board it never really read.
func TestASupersedeOnAStaleSnapshotConflictsRatherThanProceeds(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")

	// A snapshot of plan-1 taken now, handed to the ingest AFTER the real row
	// has been written by somebody else.
	snapshot := d.store.stored("plan-1")
	if err := d.store.Update(context.Background(), d.store.stored("plan-1")); err != nil {
		t.Fatalf("setup: a competing write must land: %v", err)
	}
	d.svc.repo = &staleSnapshotStore{fakePlanStore: d.store, stale: snapshot}
	planned := d.store.count()

	rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("a stale snapshot must answer 409 so the UI reloads and retries, got %d (%s)",
			rec.Code, rec.Body.String())
	}
	if n := d.store.count(); n != planned {
		t.Errorf("the store holds %d plans, want %d — a conflicted re-plan creates nothing", n, planned)
	}
}

// TestSupersedingAPlanWithNothingLeftToDoomStillTakesTheVersionCheck pins the
// branch the previous code returned early from — and it was the DANGEROUS one.
// "Nothing to recall" was read as "nothing to do", so the write that carries the
// optimistic-concurrency check never ran and a stale snapshot proceeded in
// silence. Precisely the fast path.
//
// Requirement 5 is not the same thing and is not weakened by this: a date with
// nothing live anywhere yields an EMPTY set of superseded plans, so this loop
// runs zero times and still costs zero writes. See the test above it.
func TestSupersedingAPlanWithNothingLeftToDoomStillTakesTheVersionCheck(t *testing.T) {
	store := newFakePlanStore(planForTrucks("v1"))
	svc := newTestService(store, &fakeGable{}, Config{})

	stale := store.stored("plan-1") // nothing live: this plan dooms nothing
	if err := store.Update(context.Background(), store.stored("plan-1")); err != nil {
		t.Fatalf("setup: a competing write must land: %v", err)
	}

	err := svc.supersede(context.Background(), []*Plan{stale},
		&Plan{PlanDate: "2026-06-26"}, "dispatcher@dealer.com")

	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("superseding a stale plan with nothing to recall must still 409, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// requirement 5: the frictionless path stays free
// ---------------------------------------------------------------------------

// TestReplanningADateWithNothingLiveAnywhereCostsNothing is the case this gate
// must not tax, now that it reads EVERY plan for the date rather than one.
// Re-planning a day is ordinary dispatch work: no approval prompt, no recall,
// and not one write to any plan it supersedes — including the plan that reached
// PUSHED and has since had every route recalled, which is why the gate keys on
// the ledger and not on the status.
func TestReplanningADateWithNothingLiveAnywhereCostsNothing(t *testing.T) {
	never := planForTrucks("v1", "v2") // REVIEWED: never reached the board

	recalled := planForTrucks("v3", "v4") // PUSHED, then every route withdrawn
	recalled.Status = StatusPushed
	at := timePtr()
	recalled.LiveRoutes = []LiveRoute{
		{VehicleID: "v3", VehicleName: "Flatbed 3", PushedAt: *at, RecalledAt: at, RecalledBy: "dispatcher@dealer.com"},
		{VehicleID: "v4", VehicleName: "Flatbed 4", PushedAt: *at, RecalledAt: at, RecalledBy: "dispatcher@dealer.com"},
	}

	analyzed := planForTrucks("v5") // an ordinary un-run plan
	analyzed.Status = StatusAnalyzed

	d := newDispatchDay(t, never, recalled, analyzed)
	before := map[string]*Plan{}
	for _, id := range []string{"plan-1", "plan-2", "plan-3"} {
		before[id] = d.store.stored(id)
	}
	writes := d.store.updates

	rec := d.ingest(`{"date":"2026-06-26"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("re-planning a date with nothing on the board must just work: %d %s", rec.Code, rec.Body.String())
	}
	if d.store.updates != writes {
		t.Errorf("the re-plan wrote %d time(s); a date with nothing live must cost no writes at all", d.store.updates-writes)
	}
	if len(d.g.recalled) != 0 {
		t.Errorf("nothing was live, so nothing may be recalled, got %v", d.g.recalledIDs())
	}
	for id, was := range before {
		if now := d.store.stored(id); !plansEqual(was, now) {
			t.Errorf("%s must be left alone\n before: %+v\n  after: %+v", id, was, now)
		}
	}
	if n := d.store.count(); n != 4 {
		t.Errorf("the store holds %d plans, want 4", n)
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// the wire contract: three mappings that were correct and untested
// ---------------------------------------------------------------------------

// conflictingUpdateStore fails every Update with ErrVersionConflict — the plan
// was saved by someone else between this ingest's read and its write.
type conflictingUpdateStore struct{ *fakePlanStore }

func (s conflictingUpdateStore) Update(context.Context, *Plan) error { return ErrVersionConflict }

// TestIngestErrorsKeepTheirOwnStatusOnTheWire pins the three mappings
// HandleIngest grew for this gate. Every one of them could be DELETED with the
// suite still green, and each is the difference between a sentence a dispatcher
// can act on and a 502 claiming the dealer's ERP is down.
func TestIngestErrorsKeepTheirOwnStatusOnTheWire(t *testing.T) {
	t.Run("a truck already on the road is 422, not a bogus 502", func(t *testing.T) {
		d := newDispatchDay(t, planForTrucks("v1", "v2"))
		d.push("plan-1")
		d.g.recallDispatched = true

		rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 — %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "left the yard") || !strings.Contains(body, "call the driver first") {
			t.Errorf("the dispatcher must read the real reason verbatim, got %s", body)
		}
		if strings.Contains(body, "ingest failed") {
			t.Errorf("a 502 \"ingest failed\" would blame GableLBM for a truck that is simply already out: %s", body)
		}
	})

	t.Run("a concurrent save is 409, not a bogus 502", func(t *testing.T) {
		d := newDispatchDay(t, planForTrucks("v1", "v2"))
		d.push("plan-1")
		d.svc.repo = conflictingUpdateStore{d.store}

		rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 so the UI reloads and retries — %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "ingest failed") {
			t.Errorf("a 502 would send the dispatcher to check a healthy ERP: %s", rec.Body.String())
		}
	})

	t.Run("an allowed ingest does pull the orders", func(t *testing.T) {
		// The positive control for the "gate before the ERP" assertion above:
		// without it, a gate that refused EVERYTHING would satisfy it.
		d := newDispatchDay(t)
		if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := d.g.orderDates; len(got) != 1 || got[0] != "2026-06-26" {
			t.Errorf("orders pulled for %v, want exactly one pull for the requested date", got)
		}
	})
}

// ---------------------------------------------------------------------------
// small comparisons
// ---------------------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func liveVehicleIDs(p *Plan) []string {
	out := make([]string, 0, len(p.LiveRoutes))
	for _, r := range liveRoutes(p) {
		out = append(out, r.VehicleID)
	}
	return out
}

// plansEqual compares two stored plans in full. Version and UpdatedAt are part
// of the comparison on purpose: both move on every write, so this answers "was
// this plan written at all?", which is what requirement 5 is about.
func plansEqual(a, b *Plan) bool { return reflect.DeepEqual(a, b) }
