// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Round 3 pinned the two dispatch-day properties (see date_ledger_integrity_test.go)
// against pushes that COMPLETE IN ONE CALL. Every claim made for
// clearDisplacedClaims, and every claim made for the ledger's durability, was
// asserted only on that path — so the two exits Push actually has under load
// were never in scope at all:
//
//	1. the partial-push exit, which returns BEFORE the ledger correction and
//	   whose resume then SKIPS the very trucks that owed one;
//	2. Push's own final repo.Update, which is version-checked and which loses
//	   to any concurrent write to the same plan — discarding, with it, every
//	   route already on the dealer's board.
//
// Both leave state the ERP cannot represent, and neither is a window: the
// first is permanent by construction, and the second hands gateSupersede a
// date whose live routes no ledger names.
//
// Every test in this file therefore drives the real handlers and ends on
// assertAcceptance — the same oracle, applied to the failure paths.

// ---------------------------------------------------------------------------
// root cause 1: the correction is fed the wrong set, and skipped on the exit
//               that needs it
// ---------------------------------------------------------------------------

// TestAPartialPushCorrectsTheLedgerItDisplaced is the first half of the defect,
// in two POSTs.
//
// plan-2's push writes v1 — which DELETES plan-1's route for that truck
// upstream, because ReplaceDeliveryRoute is keyed (vehicle_id, scheduled_date)
// — and then dies on the next truck. The refusal returns before the ledger
// correction runs at all, so plan-1 is left claiming a route the dealer's board
// no longer holds while plan-2 claims the one that replaced it: one truck, two
// plans, which is a state the ERP cannot represent.
func TestAPartialPushCorrectsTheLedgerItDisplaced(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.assertAcceptance()

	d.g.pushErrAfter = d.g.pushCalls + 1 // v1 lands, the next truck does not
	rec := d.post("/api/v1/workflow/plans/plan-2/push", ``)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a partial push must be reported to the dispatcher: %d (%s)", rec.Code, rec.Body.String())
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v1"}) {
		t.Fatalf("setup: plan-2 claims %v, want just the truck that landed", got)
	}
	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Errorf("plan-1 still claims %v after a partial push took that truck over — the trucks a push DID write have already destroyed the other plan's route upstream, and returning before the correction leaves that ledger lying", got)
	}
	d.assertAcceptance()
}

// TestAResumeCorrectsALedgerAnEarlierAttemptDisplaced is the second half, and
// it is why the correction cannot be fed the trucks THIS attempt wrote.
//
// The resume skips v1 — it is already live, byte for byte — so v1 is not in the
// resumed attempt's written set and the correction it owes plan-1 is never
// attempted again by anybody. The attempt that wrote it returned early; the
// attempt that could fix it cannot see it. The double claim is permanent, not a
// window.
func TestAResumeCorrectsALedgerAnEarlierAttemptDisplaced(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1", "v2"))
	d.push("plan-1")

	d.g.pushErrAfter = d.g.pushCalls + 1
	if rec := d.post("/api/v1/workflow/plans/plan-2/push", ``); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("setup: the partial push must be reported, got %d (%s)", rec.Code, rec.Body.String())
	}
	d.g.pushErrAfter = 0

	sent := d.g.pushCalls
	d.push("plan-2") // the resume: v1 is skipped, v2 is written

	if wrote := d.g.pushCalls - sent; wrote != 1 {
		t.Errorf("the resume wrote %d route(s), want 1 — v1 is already live byte for byte and must be skipped", wrote)
	}
	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Errorf("plan-1 STILL claims %v after the resume — the correction is owed for a truck the resume skipped, so feeding it only the trucks this attempt wrote can never pay it", got)
	}
	d.assertAcceptance()
}

// TestAGhostClaimNeverRecallsAnotherPlansLiveRoute is why the double claim
// above is not cosmetic.
//
// The recall path's wire key is (vehicle, date) — not "the route this plan
// pushed". A plan left claiming a truck another plan has since taken therefore
// recalls SOMEBODY ELSE'S live route the next time it drops that truck, which
// is verbatim the harm clearDisplacedClaims exists to close.
func TestAGhostClaimNeverRecallsAnotherPlansLiveRoute(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1", "v2"))
	d.push("plan-1")

	d.g.pushErrAfter = d.g.pushCalls + 1
	if rec := d.post("/api/v1/workflow/plans/plan-2/push", ``); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("setup: the partial push must be reported, got %d (%s)", rec.Code, rec.Body.String())
	}
	d.g.pushErrAfter = 0
	d.push("plan-2") // v1 + v2 are now plan-2's, on the board

	// v1 leaves the yard's fleet, so re-assigning plan-1 drops it. If plan-1
	// still claims v1, the drop recalls it — and what comes off the dispatch
	// board is plan-2's live route.
	d.g.vehicles = d.g.vehicles[1:]
	rec := d.post("/api/v1/workflow/plans/plan-1/assign", `{"override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-assigning an approved plan must proceed: %d (%s)", rec.Code, rec.Body.String())
	}

	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("re-assigning plan-1 recalled %v — plan-1 has held no route since plan-2 replaced it, and this took a LIVE route belonging to another plan off the dealer's board", got)
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// root cause 2: Push writes to the ERP, then persists — and loses the ledger
// ---------------------------------------------------------------------------

// TestAPushWhoseFinalSaveLosesAVersionRaceKeepsItsLedger drives the race
// through the real handlers, with no hook into Push at all: a dispatcher locks
// the run while the push is out at the ERP.
//
// The plan was read at the top of Push and has since crossed several ERP
// round-trips. The lock bumps its version, `UPDATE ... WHERE version=$n`
// affects zero rows, and the old code returned that conflict with both routes
// already on the dealer's board and the ledger naming them thrown away. A
// ledger append is not a conflicting edit and must not be lost to one.
func TestAPushWhoseFinalSaveLosesAVersionRaceKeepsItsLedger(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	// Fires once, immediately before Push's own CONFIRMING save lands — the
	// skip steps over the claim Push now publishes before it calls the ERP, so
	// this stays "a dispatcher locked the run while the push was out at the
	// ERP" rather than "before it had sent anything".
	d.store.beforeUpdateSkip = 1
	d.store.beforeUpdate = func() {
		rec := d.post("/api/v1/workflow/plans/plan-1/lock", `{"locked":true,"locked_by":"dispatcher@dealer.com"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("setup: the competing lock must land: %d (%s)", rec.Code, rec.Body.String())
		}
	}

	rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)

	if rec.Code != http.StatusOK {
		t.Fatalf("a push that lost the save race answered %d (%s) — the routes are on the board either way, so the ledger has to reach the store", rec.Code, rec.Body.String())
	}
	stored := d.store.stored("plan-1")
	if got := sorted(liveVehicleIDs(stored)); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("plan-1's ledger names %v, want both routes it actually put on the board", got)
	}
	if stored.Status != StatusPushed {
		t.Errorf("plan-1 is %q, want %q — the status must describe the dispatch board", stored.Status, StatusPushed)
	}
	// The re-apply must not clobber the writer it lost to.
	if stored.Lock == nil || !stored.Lock.Locked {
		t.Errorf("the competing lock was discarded by the retry: %+v", stored.Lock)
	}
	// The per-truck acks are replayed too, or the next resume re-sends routes
	// that are already on the board byte for byte.
	for _, l := range stored.Loads {
		if l.PushedAt == nil || l.PushedDigest == "" {
			t.Errorf("truck %s came back with no push ack (at=%v digest=%q) — the resume skip reads both", l.VehicleName, l.PushedAt, l.PushedDigest)
		}
	}
	d.assertAcceptance()
}

// TestAPushThatCanNeverRecordItselfLeavesNoOrphan is the same race made
// deterministic and unwinnable, using the repo's own conflictingUpdateStore.
//
// This is the orphan the whole branch exists to remove, reached from the
// inside: routes live on the dispatch board that NO plan's ledger names, so
// nothing can recall them and gateSupersede — which reads ledgers — will wave
// the next re-ingest straight over them.
func TestAPushThatCanNeverRecordItselfLeavesNoOrphan(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.svc.repo = conflictingUpdateStore{d.store}

	rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)

	if rec.Code != http.StatusConflict {
		t.Fatalf("a save that cannot be made to land must reach the dispatcher as 409: %d (%s)", rec.Code, rec.Body.String())
	}
	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Fatalf("nothing was recorded, so plan-1 must claim nothing, got %v", got)
	}
	if got := d.g.pushedIDs(); len(got) != 0 {
		t.Errorf("the dispatch board holds %v that no ledger anywhere names — a push that cannot record what it did must not leave it there", got)
	}
	d.assertAcceptance()
}

// TestAPartialPushThatAlsoLosesTheVersionRaceKeepsItsLedger crosses the two
// failure paths. The partial exit persists too, and that save was never
// version-safe either — it only logged.
func TestAPartialPushThatAlsoLosesTheVersionRaceKeepsItsLedger(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1", "v2"))
	d.push("plan-1")

	d.store.beforeUpdateSkip = 1 // step over the push's own write-ahead claim
	d.store.beforeUpdate = func() {
		rec := d.post("/api/v1/workflow/plans/plan-2/lock", `{"locked":true,"locked_by":"dispatcher@dealer.com"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("setup: the competing lock must land: %d (%s)", rec.Code, rec.Body.String())
		}
	}
	d.g.pushErrAfter = d.g.pushCalls + 1

	rec := d.post("/api/v1/workflow/plans/plan-2/push", ``)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the dispatcher must still read the push failure, not the bookkeeping one: %d (%s)", rec.Code, rec.Body.String())
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-2's ledger names %v, want the one route it did put on the board — a partial push that loses the save race orphans exactly what it wrote", got)
	}
	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Errorf("plan-1 still claims %v", got)
	}
	d.assertAcceptance()
}

// ---------------------------------------------------------------------------
// an abandoned correction
// ---------------------------------------------------------------------------

// conflictingOnStore conflicts every write to ONE named plan and passes the
// rest through — a plan another actor is saving in a tight loop, which is what
// exhausts tombstoneDisplaced's retries.
type conflictingOnStore struct {
	*fakePlanStore
	id string
}

func (s conflictingOnStore) Update(ctx context.Context, p *Plan) error {
	if p.ID == s.id {
		return ErrVersionConflict
	}
	return s.fakePlanStore.Update(ctx, p)
}

// TestALedgerCorrectionAbandonedLeavesNoDoubleClaim pins the exit
// tombstoneDisplaced takes when its retries run out.
//
// Push returned 409 there having ALREADY persisted the pushing plan with its
// new claims — so the correction it gave up on left exactly the double claim it
// was called to prevent, and the 409 told the dispatcher nothing had happened.
// A push that could not make its claim exclusive must not assert it.
func TestALedgerCorrectionAbandonedLeavesNoDoubleClaim(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.svc.repo = conflictingOnStore{fakePlanStore: d.store, id: "plan-1"}

	rec := d.post("/api/v1/workflow/plans/plan-2/push", ``)

	if rec.Code != http.StatusConflict {
		t.Fatalf("a correction that could not be made to land must reach the dispatcher as 409: %d (%s)", rec.Code, rec.Body.String())
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v1"}) {
		t.Fatalf("setup: plan-1's ledger is unwritable, so it must still claim %v, got %v", []string{"v1"}, got)
	}
	if got := liveVehicleIDs(d.store.stored("plan-2")); contains(got, "v1") {
		t.Errorf("plan-2 claims %v including the truck whose rival claim it failed to clear — an abandoned correction must not leave the plan asserting what it could not make true", got)
	}
	d.assertAcceptance()
}

func contains(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

// TestAPushOverruledByAConcurrentRepackStillRecordsItsRoutes is the other half
// of the replay rule, and the line it must not cross.
//
// A conflict is replayed because a LEDGER APPEND is a record of what is on the
// dealer's board — true whatever else happened to the plan. A TRANSITION is
// not: it is a decision taken against gates this push evaluated, and a
// concurrent re-pack has just invalidated the review those gates read. So the
// routes are recorded (something has to be able to recall them) and the push
// still answers 409 — it does not come back PUSHED off a status nobody
// re-checked.
func TestAPushOverruledByAConcurrentRepackStillRecordsItsRoutes(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	d.store.beforeUpdateSkip = 1 // step over the push's own write-ahead claim
	d.store.beforeUpdate = func() {
		// The re-pack carries an approval because by this point the push has
		// PUBLISHED its claim on both trucks, and a claim published ahead of
		// the wire call counts as live for every gate in this package — the
		// route may already be on the dealer's board and no gate can tell.
		// Re-packing over it is exactly the 423 the override idiom exists for.
		rec := d.post("/api/v1/workflow/plans/plan-1/pack", `{"override":true,"approved_by":"dispatcher@dealer.com"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("setup: the competing re-pack must land: %d (%s)", rec.Code, rec.Body.String())
		}
	}

	rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)

	if rec.Code != http.StatusConflict {
		t.Fatalf("a push whose review was invalidated under it must answer 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	stored := d.store.stored("plan-1")
	if stored.Status != StatusPacked {
		t.Errorf("plan-1 is %q, want %q — the re-pack must survive, and this push must not advance a status nobody re-gated", stored.Status, StatusPacked)
	}
	if got := sorted(liveVehicleIDs(stored)); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("plan-1's ledger names %v, want both routes it really did put on the dealer's board — refusing the transition is not a reason to forget them", got)
	}
	d.assertAcceptance()
}

// TestOneUnwritableLedgerDoesNotStrandTheOthers pins that the correction pass
// finishes.
//
// Returning on the first plan it could not correct left every LATER displaced
// plan lying for a reason that had nothing to do with it. Here plan-2 is
// unwritable and plan-1 is perfectly writable, and the ERP order visits plan-2
// first: plan-1's stale claim on v1 has to be cleared anyway, and plan-3 gives
// up only the one truck it could not make exclusive.
func TestOneUnwritableLedgerDoesNotStrandTheOthers(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v2"), planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.push("plan-2")
	d.svc.repo = conflictingOnStore{fakePlanStore: d.store, id: "plan-2"}

	rec := d.post("/api/v1/workflow/plans/plan-3/push", ``)

	if rec.Code != http.StatusConflict {
		t.Fatalf("the uncorrectable ledger must still be reported: %d (%s)", rec.Code, rec.Body.String())
	}
	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Errorf("plan-1 still claims %v — its route was destroyed upstream and its correction is writable; stopping at plan-2 is what left it lying", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-3"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-3 claims %v, want just v1 — it must give up exactly the truck whose rival claim still stands, and keep the one it cleared", got)
	}
	d.assertAcceptance()
}

// listErrStore fails the by-date read the ledger correction depends on.
type listErrStore struct {
	*fakePlanStore
	err error
}

func (s listErrStore) ListForDate(context.Context, string) ([]*Plan, error) { return nil, s.err }

// TestACorrectionThatCouldNotEvenLookWritesNothing is the boundary on giving
// claims up, restated the only way write-ahead leaves open.
//
// The old shape of this test asserted that a push whose by-date lookup failed
// KEPT its own claims, and it was seeded with ONE PLAN ON THE DATE — so the
// boundary it guarded could not produce the double claim it is a boundary on.
// With a rival present, the same store double persisted plan-2 holding v1 while
// plan-1 still held it: one transient database read, no concurrency, no ERP
// failure, GableLBM up throughout, and a stored double claim.
//
// Inverting the order removes the dilemma rather than picking a side. The
// lookup is now a PRECONDITION: it happens before the first wire call, so a
// push that cannot tell who else holds these trucks has not yet displaced
// anybody, and handing the reservation back costs nothing and strands nothing.
func TestACorrectionThatCouldNotEvenLookWritesNothing(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.assertAcceptance()

	sent := d.g.pushCalls
	d.svc.repo = listErrStore{fakePlanStore: d.store, err: errors.New("dial tcp: connection refused")}

	rec := d.post("/api/v1/workflow/plans/plan-2/push", ``)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the failed lookup must be reported: %d (%s)", rec.Code, rec.Body.String())
	}
	if wrote := d.g.pushCalls - sent; wrote != 0 {
		t.Errorf("the push wrote %d route(s) to the dealer's board without being able to tell who else holds those trucks — the lookup has to be a precondition, not an apology", wrote)
	}
	if got := liveVehicleIDs(d.store.stored("plan-2")); len(got) != 0 {
		t.Errorf("plan-2 claims %v after writing nothing — a push that could not check must not keep a claim it never made exclusive and never used", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-1 claims %v, want v1 — nothing displaced it, so nothing may take its claim away", got)
	}
	d.assertAcceptance()

	// And it is a refusal, not a wedge: the plain retry finishes the run.
	d.svc.repo = d.store
	d.push("plan-2")
	d.assertAcceptance()
	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Errorf("plan-1 still claims %v after plan-2 took both trucks over", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("plan-2 claims %v, want both trucks", got)
	}
}

// flakyListStore fails the by-date read the ledger correction depends on, until
// the test lets it recover — a transient database read, which is the ordinary
// way a correction gets missed without anybody doing anything wrong.
type flakyListStore struct {
	*fakePlanStore
	err error
}

func (s *flakyListStore) ListForDate(ctx context.Context, date string) ([]*Plan, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.fakePlanStore.ListForDate(ctx, date)
}

// TestACorrectionMissedDuringAnOutageIsNeverOwed replaces a test that PASSED
// THROUGH the violating state.
//
// Its predecessor let the outage push land on v1, leaving plan-1 claiming a
// route that push had destroyed, and called assertAcceptance only after a later
// resume had cleaned it up — so its own midpoint failed the oracle and the
// suite never asked. "The next attempt pays the arrears" is not a fix: nothing
// guarantees a next attempt, and the recall path fires first, so between the
// two calls a plain re-assign of plan-1 would take plan-2's live route off the
// dealer's board.
//
// With the lookup ahead of the wire call the arrears are never incurred. The
// oracle is therefore asserted after EVERY step, including the one in the
// middle, and the middle is now a state a dispatcher can sit in indefinitely.
func TestACorrectionMissedDuringAnOutageIsNeverOwed(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"), planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.assertAcceptance()

	repo := &flakyListStore{fakePlanStore: d.store, err: errors.New("dial tcp: connection refused")}
	d.svc.repo = repo

	if rec := d.post("/api/v1/workflow/plans/plan-2/push", ``); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the outage must be reported, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v1"}) {
		t.Errorf("the board holds %v, want just plan-1's v1 — nothing may reach the dealer while the date cannot be read", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-1"))); !equalStrings(got, []string{"v1"}) {
		t.Errorf("plan-1 claims %v, want v1 — its route is untouched, so its ledger must be too", got)
	}
	// THE MIDPOINT. The old test walked past this line without asking.
	d.assertAcceptance()

	// The database comes back. Nothing else is different: this is the plain retry.
	repo.err = nil
	d.push("plan-2")

	if got := liveVehicleIDs(d.store.stored("plan-1")); len(got) != 0 {
		t.Errorf("plan-1 still claims %v after plan-2 replaced its route upstream", got)
	}
	if got := sorted(liveVehicleIDs(d.store.stored("plan-2"))); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("plan-2 claims %v, want both trucks", got)
	}
	d.assertAcceptance()
}
