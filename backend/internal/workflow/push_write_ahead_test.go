// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Four rounds fixed the root cause they were given and left the same harm
// reachable: a route live on the dealer's board that no ledger names, or one
// truck claimed by two plans. Every one of them patched the CORRECTION. This
// file is about the ORDER.
//
// Push used to publish its own claim LAST:
//
//	PushDeliveryRoute     the truck is MINE upstream (the rival's route DELETEd)
//	clearDisplacedClaims  correct every RIVAL ledger
//	persistPush           publish MY OWN claim                      <- last
//
// Between the first line and the last, this plan owned a route upstream that NO
// STORED LEDGER NAMED. A rival push whose own correction ran inside that gap
// looked, saw nothing to correct, and both plans persisted a live claim on the
// same truck. Widening the correction's INPUT SET (round 4) did nothing about
// its VISIBILITY WINDOW, and lengthened the window.
//
// The window cannot be removed — there is no distributed transaction across
// AI_LM and GableLBM — so it is pointed the other way. The claim is published
// as PENDING before the wire call and confirmed after, which makes the gap
// "claimed, not yet live upstream" instead of "live upstream, claimed by
// nobody". The first is an over-claim, which this package already treats as the
// safe direction; the second is the harm itself.
//
// Every test here drives the real handlers and asserts the acceptance oracle
// after EVERY step. A scenario that transits an invalid state has found a bug,
// not demonstrated recovery.

// ---------------------------------------------------------------------------
// stores
// ---------------------------------------------------------------------------

// conflictAfterStore lets the first n writes land and conflicts every write
// after them, forever.
//
// It exists because Push now writes TWICE — the claim, then the confirmation —
// and the interesting failure is the one that loses the second write having
// already landed the first. A store that conflicts everything cannot reach it:
// the reservation fails, and the push never touches the ERP at all.
type conflictAfterStore struct {
	*fakePlanStore
	n int
}

func (s *conflictAfterStore) Update(ctx context.Context, p *Plan) error {
	if s.n <= 0 {
		return ErrVersionConflict
	}
	s.n--
	return s.fakePlanStore.Update(ctx, p)
}

// ---------------------------------------------------------------------------
// the ordering itself
// ---------------------------------------------------------------------------

// TestTheClaimIsPublishedBeforeTheWireCall pins the inversion at its narrowest
// point, from inside the window, with no concurrency at all.
//
// The ERP double is asked, at the moment of the very first PushDeliveryRoute,
// what the STORE says about the truck it is being handed. Before the inversion
// the answer was "nothing" — that emptiness is the whole defect, because it is
// exactly what a rival push's correction reads. After it, the claim is already
// there.
func TestTheClaimIsPublishedBeforeTheWireCall(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	var atWireTime []string
	d.g.onFirstPush = func() {
		atWireTime = sorted(liveVehicleIDs(d.store.stored("plan-1")))
	}

	d.push("plan-1")

	if len(atWireTime) == 0 {
		t.Fatalf("at the instant the first route reached GableLBM the stored ledger named %v — a rival push correcting itself inside this window sees nothing to correct, takes the truck, and both plans end up claiming it", atWireTime)
	}
	if !equalStrings(atWireTime, []string{"v1", "v2"}) {
		t.Errorf("the reservation named %v, want every truck this attempt may write: a truck written before its claim is published is unnamed for as long as the write takes", atWireTime)
	}
	d.assertAcceptance()
}

// TestAPublishedClaimIsPendingUntilTheRouteIsConfirmed pins the two states
// apart, because a design in which they are the same value is the design
// before this one.
func TestAPublishedClaimIsPendingUntilTheRouteIsConfirmed(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	var atWireTime []LiveRoute
	d.g.onFirstPush = func() {
		atWireTime = d.store.stored("plan-1").LiveRoutes
	}
	d.push("plan-1")

	for _, r := range atWireTime {
		if !r.Pending() {
			t.Errorf("truck %s was already recorded %q before its route reached the ERP — a claim that reads CONFIRMED before the wire call would let the resume skip a truck that was never sent", r.VehicleID, r.State)
		}
	}
	for _, r := range d.store.stored("plan-1").LiveRoutes {
		if !r.Confirmed() {
			t.Errorf("truck %s is still %q after a push that succeeded — an intent nobody promotes blocks every other plan until its lease runs out", r.VehicleID, r.State)
		}
	}
	d.assertAcceptance()
}

// TestARivalPushWaitsForAClaimAlreadyPublished is the mechanism the inversion
// buys, made deterministic: a second push that arrives inside the first one's
// window now SEES the claim, and stands down instead of taking the truck.
//
// The hook fires while plan-1 is out at the ERP — after its claim is published,
// before it is confirmed — which is precisely the interval in which two plans
// used to end up holding one truck.
func TestARivalPushWaitsForAClaimAlreadyPublished(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1"))

	var rival int
	var rivalBody string
	d.g.onFirstPush = func() {
		rec := d.post("/api/v1/workflow/plans/plan-2/push", ``)
		rival, rivalBody = rec.Code, rec.Body.String()
	}

	d.push("plan-1")

	if rival != http.StatusConflict {
		t.Fatalf("the rival push answered %d (%s), want 409 — a push landing inside another push's window must stand down, not take the truck", rival, rivalBody)
	}
	if got := liveVehicleIDs(d.store.stored("plan-2")); len(got) != 0 {
		t.Errorf("plan-2 claims %v after standing down — a push that refused must hand back every claim it published", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-1 claims %v, want v1", got)
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// B1: the correction that could not look
// ---------------------------------------------------------------------------
//
// See TestACorrectionThatCouldNotEvenLookWritesNothing in
// push_ledger_durability_test.go: the boundary test that missed this is fixed
// there, seeded with the rival it needed, alongside the outage test that used
// to pass THROUGH the violating state.

// ---------------------------------------------------------------------------
// B2: a withdrawal that stopped at the first truck
// ---------------------------------------------------------------------------

// TestAWithdrawalFinishesThePassItStarted is the abort-on-first-truck rule in
// the one place it is wrong.
//
// recallRoutes returns on the first truck it cannot take back, and for its own
// callers that is right: they abort and persist nothing, so a half-recalled
// board the plan has half-forgotten is the worse outcome. The withdrawal of a
// push that could not record itself has nothing to fall back to — these routes
// are on the dealer's board with no ledger naming them — so stopping at the
// first truck abandons every truck AFTER it for a reason that has nothing to do
// with them. That is round 4's own M4 defect, reintroduced on the new path.
func TestAWithdrawalFinishesThePassItStarted(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	// The reservation lands and is then LOST, so no ledger names what the push
	// writes; every later write conflicts, so it can never record itself either.
	d.svc.repo = &amnesiacConflictStore{store: d.store, allow: 1}
	d.g.recallErrFor = map[string]error{"v1": errors.New("gable POST /recall: status 503: upstream unavailable")}

	rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)

	if rec.Code != http.StatusConflict {
		t.Fatalf("a push that can never record itself must reach the dispatcher as 409: %d (%s)", rec.Code, rec.Body.String())
	}
	if !contains(d.g.recalledIDs(), "v2") {
		t.Errorf("the withdrawal attempted %v — it stopped at the truck GableLBM would not take back and never even tried v2, which is now live on the dealer's board with no ledger naming it", d.g.recalledIDs())
	}
	if contains(sorted(d.g.pushedIDs()), "v2") {
		t.Errorf("the board still holds %v — v2 could have come off and did not", sorted(d.g.pushedIDs()))
	}
	// v1 is stranded because GableLBM ITSELF refused to take it back. That is
	// the one orphan this system cannot close, and it is named here rather than
	// waved through.
	d.assertAcceptanceExceptStranded("GableLBM refused the withdrawal of v1", "v1")
}

// amnesiacConflictStore lands the first `allow` writes with the live-route
// ledger STRIPPED OUT, then conflicts forever: a reservation that does not
// survive its own write, and a confirmation that can never land.
//
// The amnesia is not hypothetical. repository.go carries a long warning that a
// plan-level field missing from its `payload` struct is silently dropped on
// every write and that nothing in the in-memory suite would notice. It is the
// one way the reservation this whole design leans on can fail to be there
// afterwards, so the withdrawal path has to survive it.
type amnesiacConflictStore struct {
	store *fakePlanStore
	allow int
}

func (s amnesiacConflictStore) Create(ctx context.Context, p *Plan) error {
	return s.store.Create(ctx, p)
}
func (s amnesiacConflictStore) Get(ctx context.Context, id string) (*Plan, error) {
	return s.store.Get(ctx, id)
}
func (s amnesiacConflictStore) GetLatestForDate(ctx context.Context, d string) (*Plan, error) {
	return s.store.GetLatestForDate(ctx, d)
}
func (s amnesiacConflictStore) ListForDate(ctx context.Context, d string) ([]*Plan, error) {
	return s.store.ListForDate(ctx, d)
}
func (s *amnesiacConflictStore) Update(ctx context.Context, p *Plan) error {
	if s.allow <= 0 {
		return ErrVersionConflict
	}
	s.allow--
	stripped := clonePlan(p)
	stripped.LiveRoutes = nil
	if err := s.store.Update(ctx, stripped); err != nil {
		return err
	}
	p.Version = stripped.Version
	return nil
}

// TestAPushThatCannotConfirmItselfIsStillNamedByItsOwnClaim is the same
// terminal failure WITHOUT the amnesia, and it is why the withdrawal above is
// almost never reached any more.
//
// The claim was published and saved before the wire call, so a lost
// CONFIRMATION is not a lost ledger: the routes are named, recallable and
// visible to every gate. Taking them off the dealer's board here would be the
// mirror lie from the other side — a stored claim pointing at a route that is
// no longer there — so nothing is withdrawn, and the plain retry finishes the
// run.
func TestAPushThatCannotConfirmItselfIsStillNamedByItsOwnClaim(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	repo := &conflictAfterStore{fakePlanStore: d.store, n: 1} // the reservation, and nothing else
	d.svc.repo = repo

	rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("the board holds %v, want both — the routes are named by the reservation, so withdrawing them would strand a stored claim", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("plan-1's ledger names %v, want both routes it really did put on the board", got)
	}
	if len(d.g.recalledIDs()) != 0 {
		t.Errorf("the push withdrew %v from the dealer's board while its own stored claim still named them", d.g.recalledIDs())
	}
	d.assertAcceptance()

	// The retry finishes it: a PENDING claim never satisfies the resume skip,
	// so the routes are re-sent and confirmed.
	repo.n = 99
	d.push("plan-1")
	for _, r := range d.store.stored("plan-1").LiveRoutes {
		if !r.Confirmed() {
			t.Errorf("truck %s is still %q after the retry — the run cannot be left holding intents nobody resolves", r.VehicleID, r.State)
		}
	}
	d.assertAcceptance()
}

// TestATruckClaimedAndNeverWrittenDisplacesNobody is the other edge of the
// write-ahead bargain, and the reason the ledger correction is keyed on what
// this plan has CONFIRMED rather than on what it reserved.
//
// A published claim is an intent, not a displacement. The rival ledger it would
// correct is only lying once this push has actually replaced the route upstream
// — GableLBM's ReplaceDeliveryRoute is what destroys the other plan's route, and
// a truck this push never reached destroyed nothing. Correcting on the reserved
// set would tombstone a rival that still holds a real route, leaving it live on
// the dealer's board with no ledger naming it: the orphan, manufactured by the
// correction meant to prevent it.
//
// The same call also has to give the unwritten claim straight back, or the
// over-claim this ordering accepts INSIDE the window would outlive the window.
func TestATruckClaimedAndNeverWrittenDisplacesNobody(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v2"), planForTrucks("v1", "v2"))
	d.push("plan-1") // plan-1 really holds v2
	d.assertAcceptance()

	d.g.pushErrAfter = d.g.pushCalls + 1 // plan-2 lands v1 and never reaches v2

	rec := d.post("/api/v1/workflow/plans/plan-2/push", ``)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a partial push must be reported to the dispatcher: %d (%s)", rec.Code, rec.Body.String())
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v2"}) {
		t.Errorf("plan-1 claims %v, want v2 — plan-2 claimed that truck and never wrote it, so nothing displaced plan-1 and its route is still on the dealer's board", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-2 claims %v, want just v1 — a truck it claimed ahead of a wire call it never made must have the claim handed straight back", got)
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// B3: a replay must not resurrect a claim that is no longer this plan's
// ---------------------------------------------------------------------------

// TestAReplayDoesNotResurrectAClaimARivalTookOver pins round 4's regression.
//
// persistPush re-reads on a version conflict and re-states the outcome on the
// fresh copy. Round 4 re-stated the ledger unconditionally, so a truck a rival
// had taken over inside the window came back as this plan's claim: at ef59ac8
// the 409 correctly discarded it, at 17b11ed it was re-asserted, and the date
// was left with two plans holding one truck.
//
// The takeover is reached here through the claim LEASE, which is the only way
// a rival can displace a published intent: plan-1's claim is expired by
// configuration, so plan-2 treats it exactly as it would a confirmed one.
func TestAReplayDoesNotResurrectAClaimARivalTookOver(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1"))
	d.svc.cfg.PendingClaimLease = time.Nanosecond

	// Fires inside plan-1's window, after its claim is published and before it
	// is confirmed: a rival takes v1, and a dispatcher's lock makes plan-1's
	// own confirming save lose the version race so the replay runs.
	d.g.onFirstPush = func() {
		if rec := d.post("/api/v1/workflow/plans/plan-2/push", ``); rec.Code != http.StatusOK {
			t.Errorf("setup: the rival push must land past the expired claim: %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := d.post("/api/v1/workflow/plans/plan-1/lock", `{"locked":true,"locked_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusOK {
			t.Errorf("setup: the competing lock must land: %d (%s)", rec.Code, rec.Body.String())
		}
	}

	rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)
	t.Logf("plan-1's push answered %d (%s)", rec.Code, rec.Body.String())

	if got := liveVehicleIDs(d.store.stored("plan-1")); contains(got, "v1") {
		t.Errorf("plan-1 claims %v — a rival tombstoned that claim and took the truck, and the replay put it back; a version conflict used to discard it and the replay must not undo that", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-2 claims %v, want v1 — it holds the route that is actually on the board", got)
	}
	if len(d.g.recalledIDs()) != 0 {
		t.Errorf("plan-1 withdrew %v — the route it disowned belongs to plan-2 now, and taking it off the board is the same harm from the other side", d.g.recalledIDs())
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// a claim nobody resolved: the crash case
// ---------------------------------------------------------------------------

// crashedClaim is what a process that died between publishing its claim and
// resolving it leaves in the database: one PENDING row, and nothing running
// that will ever come back for it.
func crashedClaim(p *Plan, vehicleID, vehicleName string, at time.Time) {
	t := at
	p.LiveRoutes = append(p.LiveRoutes, LiveRoute{
		VehicleID: vehicleID, VehicleName: vehicleName, State: ClaimPending, ClaimedAt: &t,
	})
}

// TestACrashedClaimIsHealedByItsOwnPlansNextPush is the first of the three
// resolutions, and the one that needs nobody to wait.
//
// A PENDING claim never satisfies the resume's "already sent, unchanged" skip,
// precisely because nothing knows whether the wire call happened. So the plan
// that owns the claim re-sends the route and confirms it, and the ledger and
// the board agree again.
func TestACrashedClaimIsHealedByItsOwnPlansNextPush(t *testing.T) {
	dead := planForTrucks("v1", "v2")
	d := newDispatchDay(t, dead)
	// The process died having claimed both trucks and written neither. It could
	// equally have written both; the plan cannot tell, and does not need to.
	stored := d.store.stored("plan-1")
	crashedClaim(stored, "v1", "Flatbed 1", time.Now().Add(-time.Hour))
	crashedClaim(stored, "v2", "Flatbed 2", time.Now().Add(-time.Hour))
	if err := d.store.Update(context.Background(), stored); err != nil {
		t.Fatalf("setup: %v", err)
	}

	d.push("plan-1")

	if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("the board holds %v, want both — an unresolved claim must always re-send, because nothing knows whether the wire call happened", got)
	}
	for _, r := range d.store.stored("plan-1").LiveRoutes {
		if !r.Confirmed() {
			t.Errorf("truck %s is still %q after its own plan pushed again", r.VehicleID, r.State)
		}
	}
	d.assertAcceptance()
}

// TestACrashedClaimCannotWedgeTheDateForever is the second resolution, and it
// is the one that makes the first optional.
//
// A published claim blocks other plans — that is the whole point of publishing
// it — so a claim nothing will ever resolve would hold a truck for the rest of
// the day. It holds it for one LEASE. Inside the lease the rival stands down
// and is told to try again; past it the rival displaces the stale claim exactly
// as it displaces a confirmed one, which is sound because its own
// ReplaceDeliveryRoute deletes whatever route is upstream for that truck anyway.
func TestACrashedClaimCannotWedgeTheDateForever(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1"))
	d.svc.cfg.PendingClaimLease = time.Hour

	dead := d.store.stored("plan-1")
	crashedClaim(dead, "v1", "Flatbed 1", time.Now())
	if err := d.store.Update(context.Background(), dead); err != nil {
		t.Fatalf("setup: %v", err)
	}
	d.assertAcceptance()

	// Inside the lease: the date is held, and the refusal says so.
	rec := d.post("/api/v1/workflow/plans/plan-2/push", ``)
	if rec.Code != http.StatusConflict {
		t.Fatalf("inside the lease a rival must stand down: %d (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Flatbed 1") {
		t.Errorf("the refusal must name the truck it is waiting on: %s", body)
	}
	if got := d.g.pushedIDs(); len(got) != 0 {
		t.Errorf("the board holds %v — a push that stood down must not have written anything", got)
	}
	d.assertAcceptance()

	// Past the lease: the same call, unchanged, goes through.
	d.svc.cfg.PendingClaimLease = time.Nanosecond
	d.push("plan-2")

	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Errorf("the crashed plan still claims %v after another plan took the truck past its lease", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-2 claims %v, want v1", got)
	}
	d.assertAcceptance()
}

// TestACrashedClaimIsVisibleToTheReplanGate is the third resolution, and the
// reason a published claim counts as live for every gate in this package.
//
// A PENDING claim MAY be on the dealer's board — that is exactly what makes it
// pending. Waving a re-plan over it would orphan a route this system can still
// see, so it takes the same 423 a confirmed route takes, and the approval
// recalls it: idempotent upstream, so it costs one redundant call when the
// route was never written and saves an orphan when it was.
func TestACrashedClaimIsVisibleToTheReplanGate(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"))
	dead := d.store.stored("plan-1")
	crashedClaim(dead, "v1", "Flatbed 1", time.Now())
	if err := d.store.Update(context.Background(), dead); err != nil {
		t.Fatalf("setup: %v", err)
	}

	rec := d.ingest(`{"date":"2026-06-26"}`)
	if rec.Code != http.StatusLocked {
		t.Fatalf("re-planning a date holding an unresolved claim answered %d, want 423 — the route may be on the dealer's board and nothing else can tell: %s", rec.Code, rec.Body.String())
	}
	d.assertAcceptance()

	rec = d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the approved re-plan must proceed: %d (%s)", rec.Code, rec.Body.String())
	}
	if !contains(d.g.recalledIDs(), "v1") {
		t.Errorf("the re-plan recalled %v — an unresolved claim has to be withdrawn upstream, because it may be a real route", d.g.recalledIDs())
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// the acceptance test: two real goroutines, two POSTs, no hook
// ---------------------------------------------------------------------------

// racePushTrial runs ONE trial: a date holding two plans that both want the
// same trucks, pushed by two goroutines at once through the real handlers, with
// no test hook anywhere inside Push. It returns the acceptance violations the
// date was left in.
func racePushTrial(t *testing.T) []string {
	t.Helper()
	d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v2"))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, id := range []string{"plan-1", "plan-2"} {
		wg.Add(1)
		go func(planID string) {
			defer wg.Done()
			<-start
			d.post("/api/v1/workflow/plans/"+planID+"/push", ``)
		}(id)
	}
	close(start)
	wg.Wait()
	return d.acceptanceViolations()
}

// raceTrials is the trial count. It is a named number and it is reported,
// because "no failures observed" without one is how a 1-in-400 defect gets
// called closed. Round 3 measured 1/400 here and round 4 measured 4-17/400.
func raceTrials() int {
	if v := os.Getenv("RACE_TRIALS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 400
}

// TestTwoConcurrentPushesNeverDoubleClaimATruck is acceptance criterion III.
func TestTwoConcurrentPushesNeverDoubleClaimATruck(t *testing.T) {
	trials := raceTrials()
	bad := 0
	seen := map[string]int{}
	for i := 0; i < trials; i++ {
		if vs := racePushTrial(t); len(vs) > 0 {
			bad++
			for _, v := range vs {
				seen[v]++
			}
		}
	}
	t.Logf("%d/%d trials violated the acceptance oracle", bad, trials)
	if bad != 0 {
		for v, n := range seen {
			t.Errorf("x%d %s", n, v)
		}
		t.Fatalf("%d of %d concurrent trials left the date in a state the ERP cannot represent", bad, trials)
	}
}

// TestTwoConcurrentPushesStillDispatch is the other half of criterion III, and
// it is what stops the test above being satisfied by refusing everything.
//
// Standing down is the safe answer to a contested truck, and BOTH racers
// standing down is possible — each publishes before it looks, so each can see
// the other — and harmless, because nothing reached the dealer and the
// dispatcher pushes again. What must not happen is that becoming the normal
// outcome: a date that never dispatches is a different outage, not a fix. Note
// that two 200s is NOT a defect here; a race that resolves into two ordinary
// sequential pushes has the second legitimately displacing the first, and the
// oracle is what says whether the date is sound.
func TestTwoConcurrentPushesStillDispatch(t *testing.T) {
	trials := raceTrials() / 4
	dispatched := 0
	for i := 0; i < trials; i++ {
		d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1"))
		var mu sync.Mutex
		codes := map[string]int{}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, id := range []string{"plan-1", "plan-2"} {
			wg.Add(1)
			go func(planID string) {
				defer wg.Done()
				<-start
				rec := d.post("/api/v1/workflow/plans/"+planID+"/push", ``)
				mu.Lock()
				codes[planID] = rec.Code
				mu.Unlock()
			}(id)
		}
		close(start)
		wg.Wait()

		for id, c := range codes {
			if c != http.StatusOK {
				continue
			}
			dispatched++
			if got := d.g.pushedIDs(); len(got) == 0 {
				t.Fatalf("%s answered 200 and the dealer's board is empty: %v", id, codes)
			}
			break
		}
		for _, v := range d.acceptanceViolations() {
			t.Fatal(v)
		}
	}
	t.Logf("%d/%d trials put a route on the dealer's board", dispatched, trials)
	if dispatched == 0 {
		t.Fatalf("no trial in %d dispatched anything — refusing every contested push satisfies the oracle and delivers nothing", trials)
	}
}
