// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// Three states the dispatch-board authority could not see, each proved through
// the real handlers.
//
// board_authority_test.go made the RE-PLAN gate read GableLBM's board. These are
// the three holes that were still open once it did, and all three end with a
// route on the dealer's board being destroyed or a claim being deleted:
//
//  1. Assign never read the board at all. Four independently-built origin states
//     answered 423 on re-ingest and 200 on re-assign — and re-assign is not a
//     passive path, it RECALLS every route whose truck the new assignment drops,
//     choosing those trucks from the ledger this whole branch exists to stop
//     trusting.
//  2. delivery_routes.status is VARCHAR(50) with no CHECK constraint. A value
//     this service did not recognise answered false to Live() and to
//     Dispatched(), so a route that was on the board RIGHT NOW read as "no such
//     route": the claim on it was tombstoned with no wire call and no approval,
//     the row was reported nowhere, and the report then said the date was in
//     sync.
//  3. Claims were matched to the board by (vehicle, date). The ERP does not keep
//     that pair unique — migration 009 declines the index, and the dealer's own
//     CreateRoute inserts with no dedup — so a dispatcher's hand-built second run
//     for a truck stood in for ours, the report read in sync with zero
//     divergences, and an approved re-plan recalled the truck and destroyed a run
//     it had never named.

// ---------------------------------------------------------------------------
// harness additions
// ---------------------------------------------------------------------------

// loseTheLedgerWrite models the process dying between the ERP write and the
// ledger write: the board keeps the route, the plan remembers nothing — neither
// the claim, nor the per-truck push ack, nor the status the push earned, because
// persistPush writes all three together.
func (d *dispatchDay) loseTheLedgerWrite(planID string) {
	d.t.Helper()
	d.store.mu.Lock()
	defer d.store.mu.Unlock()
	p := d.store.plans[planID]
	p.LiveRoutes = nil
	p.Status = StatusReviewed
	for i := range p.Loads {
		p.Loads[i].PushedAt = nil
		p.Loads[i].PushedDigest = ""
	}
}

// boardRows is the dealer's board as the ERP double would serve it, ROW BY ROW.
// d.board() answers by truck and cannot see the state these tests are about.
func (d *dispatchDay) boardRows() []gable.BoardRoute {
	d.t.Helper()
	rows, err := d.g.ListDeliveryRoutesForDate(context.Background(), d.date)
	if err != nil {
		d.t.Fatalf("board read: %v", err)
	}
	return rows
}

// boardCarries reports whether any row on the board still carries this order —
// the question "did the dispatcher's own run survive?" reduces to.
func (d *dispatchDay) boardCarries(orderID string) bool {
	d.t.Helper()
	for _, r := range d.boardRows() {
		for _, o := range r.OrderIDs {
			if o == orderID {
				return true
			}
		}
	}
	return false
}

// assign re-runs truck assignment over the wire.
func (d *dispatchDay) assign(planID, body string) *httptest.ResponseRecorder {
	d.t.Helper()
	return d.post("/api/v1/workflow/plans/"+planID+"/assign", body)
}

// liveClaim returns the plan's live ledger entry for a truck, or nil.
func liveClaim(p *Plan, vehicleID string) *LiveRoute {
	for i := range p.LiveRoutes {
		if p.LiveRoutes[i].VehicleID == vehicleID && p.LiveRoutes[i].Live() {
			return &p.LiveRoutes[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ACCEPTANCE I — a re-assignment is a writer to the board, and now reads it
// ---------------------------------------------------------------------------

// originStates are four independently-built ways a route ends up on GableLBM's
// dispatch board with no ledger of ours naming it. They are listed as data
// because the defect was not in any one of them: it was that the re-plan gate
// refused all four and the re-assign gate refused none.
var originStates = []struct {
	name  string
	setup func(d *dispatchDay)
}{
	{"a dispatcher hand-built a run in GableLBM", func(d *dispatchDay) {
		d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-written")
	}},
	{"a route written by something that is not this service", func(d *dispatchDay) {
		// DRAFT, because that is what internal/delivery/repository.go's
		// CreateRoute defaults to — the dealer's own dispatch UI.
		d.boardOnly("v7", gable.RouteStatusDraft, "o-external")
	}},
	{"a crash between the ERP write and the ledger write", func(d *dispatchDay) {
		d.push("plan-1")
		d.loseTheLedgerWrite("plan-1")
	}},
	{"a holder killed mid-push", func(d *dispatchDay) {
		d.g.pushErrAfter = 1
		d.post("/api/v1/workflow/plans/plan-1/push", ``)
		d.g.pushErrAfter = 0
		d.loseTheLedgerWrite("plan-1")
	}},
}

// TestReassigningOverARouteOnlyTheBoardKnowsNeedsTheSameApprovalAsReplanning is
// the observed defect, stated as two POSTs per origin state.
//
// Instrumented against c2cb4d9, Assign made ZERO board reads in every one of
// these states and answered 200 in all four, where the re-ingest immediately
// before it answered 423. The asymmetry has no product justification: both are
// writers to that date's dispatch board, and the re-assignment is the one that
// issues recalls.
func TestReassigningOverARouteOnlyTheBoardKnowsNeedsTheSameApprovalAsReplanning(t *testing.T) {
	for _, st := range originStates {
		t.Run(st.name, func(t *testing.T) {
			d := newDispatchDay(t, planForTrucks("v1", "v2"))
			st.setup(d)

			if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusLocked {
				t.Fatalf("control: re-ingest must answer 423 in this state, got %d: %s", rec.Code, rec.Body.String())
			}

			readsBefore := d.g.boardReads()
			rec := d.assign("plan-1", `{}`)
			if rec.Code != http.StatusLocked {
				t.Fatalf("re-assign answered %d where re-ingest answered 423 — a re-assignment recalls the routes of every truck it drops, so it may not run over a board it has never looked at: %s",
					rec.Code, rec.Body.String())
			}
			if n := d.g.boardReads() - readsBefore; n == 0 {
				t.Errorf("the re-assignment consulted GableLBM's dispatch board %d times — it is deciding which routes to withdraw from the ledger alone", n)
			}
			if !strings.Contains(rec.Body.String(), "NO plan of ours names") {
				t.Errorf("the refusal must say what is on the board that nobody named, or the approver cannot weigh it: %s", rec.Body.String())
			}
			if got := d.g.recalledIDs(); len(got) != 0 {
				t.Errorf("a refused re-assignment recalled %v — a refusal must send nothing", got)
			}
		})
	}
}

// TestAnApprovedReassignmentDoesNotWithdrawTheRouteItRanOver pins what the
// approval on THIS gate means, which is not what it means on the re-plan gate.
//
// A re-ingest replaces the whole date, so approving it withdraws the unnamed
// routes and puts a new plan in their place. A re-assignment re-shuffles one
// plan and has nothing to put in an unnamed run's place, so withdrawing one
// would cancel a delivery and leave the slot empty. The approval therefore says
// "I know that run is there and this one may be re-planned around it" — and the
// sentence the approver reads has to say so, or they will read it as the re-plan
// prompt.
func TestAnApprovedReassignmentDoesNotWithdrawTheRouteItRanOver(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-written")

	refusal := d.assign("plan-1", `{}`).Body.String()
	if !strings.Contains(refusal, "does NOT withdraw them") {
		t.Errorf("the prompt must say approving leaves these routes where they are: %s", refusal)
	}

	rec := d.assign("plan-1", `{"override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("an approved re-assignment answered %d: %s", rec.Code, rec.Body.String())
	}
	if !d.boardCarries("o-hand-written") {
		t.Fatal("the approved re-assignment withdrew the dispatcher's own run — this gate approves running BESIDE it, never cancelling it")
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("an approved re-assignment recalled %v; it may only recall trucks its own new assignment dropped", got)
	}
	// And the approval is on the plan, by name, where an incident can find it.
	stored := d.store.stored("plan-1")
	if len(stored.PushedOverrides) == 0 ||
		!strings.Contains(stored.PushedOverrides[len(stored.PushedOverrides)-1].Note, "v9") {
		t.Errorf("the approval must be recorded naming the truck it was given about: %+v", stored.PushedOverrides)
	}
}

// TestAReassignmentComputesItsRecallSetFromTheBoard is the second half of the
// same defect, and the half that destroys something.
//
// recallDroppedRoutes withdraws, by (vehicle_id, scheduled_date), every route in
// the plan's ledger whose truck the new assignment dropped. A claim the board no
// longer backs is not a route — it is a stale record — and recalling on it
// cancels whatever that truck has acquired since. Here that is a run the
// dispatcher built by hand, which is the only thing on v2 at all.
func TestAReassignmentComputesItsRecallSetFromTheBoard(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")

	// The dispatcher cancels our v2 run in GableLBM and builds their own.
	d.g.removeFromBoard("v2", d.date)
	d.boardOnly("v2", gable.RouteStatusScheduled, "o-hand-written")

	// The new assignment can only use v1, so v2 is dropped and its ledger entry
	// is what recallDroppedRoutes would act on.
	d.g.vehicles = d.g.vehicles[:1]

	rec := d.assign("plan-1", `{"override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-assign answered %d: %s", rec.Code, rec.Body.String())
	}
	for _, v := range d.g.recalledIDs() {
		if v == "v2" {
			t.Fatal("the re-assignment recalled v2 on the strength of a claim the board does not back — that recall is keyed (vehicle, date) and cancelled the run the dispatcher built there")
		}
	}
	if !d.boardCarries("o-hand-written") {
		t.Fatal("the dispatcher's run on v2 was destroyed by a re-assignment that never named it")
	}
	// The stale claim is corrected rather than left to do this again tomorrow.
	if c := liveClaim(d.store.stored("plan-1"), "v2"); c != nil {
		t.Errorf("the claim the board does not back is still live: %+v", c)
	}
}

// TestTheAssignBoardIsReadTwiceAndTheDecidingReadIsUnderTheDateClaim pins the
// PLACEMENT, which no end-state assertion can see. It is the same property, and
// the same proof, that the re-plan path carries.
//
// The cheap read is outside the claim so a refused re-assignment costs no
// serialization and no CVRP solve. The deciding read is inside it because the
// recall set is derived from that view several ERP round-trips later, and a view
// taken outside the claim is one another writer can still be moving.
func TestTheAssignBoardIsReadTwiceAndTheDecidingReadIsUnderTheDateClaim(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	held := map[int]bool{}
	var second int
	d.g.onBoardRead = func(n int) {
		held[n] = d.store.holdsDate(d.date)
		if n == 2 {
			second = d.post("/api/v1/workflow/plans/plan-1/push", ``).Code
		}
	}
	if rec := d.assign("plan-1", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}

	if n := d.g.boardReads(); n != 2 {
		t.Fatalf("the board was read %d time(s) for one re-assignment, want 2 (one to make the refusal honest before the CVRP solve, one to decide under the claim)", n)
	}
	if held[1] {
		t.Errorf("the FIRST board read holds the dispatch date — taking the claim here serializes every push behind every re-assignment")
	}
	if !held[2] {
		t.Errorf("the DECIDING board read is taken OUTSIDE the dispatch-date claim, so the recall set is computed from a board another writer can still be moving")
	}
	if second != http.StatusConflict {
		t.Errorf("a competing writer for this date answered %d from inside the deciding read, want 409", second)
	}

	// A refused re-assignment costs ONE board read and never reaches the fleet.
	d2 := newDispatchDay(t, planForTrucks("v1", "v2"))
	d2.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-written")
	reads := d2.g.boardReads()
	if rec := d2.assign("plan-1", `{}`); rec.Code != http.StatusLocked {
		t.Fatalf("want 423, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := d2.g.boardReads() - reads; n != 1 {
		t.Errorf("a refused re-assignment read the board %d time(s), want 1 — it is refused before the deciding read is reached", n)
	}
	if grants, _ := d2.store.lockCounts(); grants != 0 {
		t.Errorf("a refused re-assignment took the dispatch-date claim %d time(s); the cheap ask exists precisely so it does not", grants)
	}
}

// TestAReassignmentIsRefusedWhenTheDispatchBoardCannotBeRead is requirement VI
// on the path that just acquired a board read. Failing OPEN here would restore
// the entire harm class on a network blip.
func TestAReassignmentIsRefusedWhenTheDispatchBoardCannotBeRead(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.g.boardErr = errUpstreamDown

	before := d.store.stored("plan-1")
	rec := d.assign("plan-1", `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("an unreadable board answered %d, want 502 — and never 423, because there is nothing here a dispatcher could approve: %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "will not plan over a board it cannot see") {
		t.Errorf("the refusal must say why nothing happened: %s", rec.Body.String())
	}
	after := d.store.stored("plan-1")
	if after.Version != before.Version || after.Status != before.Status {
		t.Errorf("a refused re-assignment wrote to the plan: %d/%s -> %d/%s",
			before.Version, before.Status, after.Version, after.Status)
	}
}

// TestATruckInTransitStillStopsAReassignment is requirement IV on this path. A
// departed truck is HELD, not a ghost, so its claim survives the repair, gates
// the re-assignment, and — if the assignment drops it — produces the terminal
// refusal rather than a recall nobody can honour.
func TestATruckInTransitStillStopsAReassignment(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.g.setBoardStatus("v2", gable.RouteStatusInTransit)
	d.g.vehicles = d.g.vehicles[:1] // v2 is dropped by the new assignment

	if c := liveClaim(d.store.stored("plan-1"), "v2"); c == nil {
		t.Fatal("setup: the claim on the departed truck must still be live")
	}
	rec := d.assign("plan-1", `{"override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("re-assigning away a truck that has left the yard answered %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "call the driver") {
		t.Errorf("the refusal must be the terminal one: %s", rec.Body.String())
	}
	if c := liveClaim(d.store.stored("plan-1"), "v2"); c == nil {
		t.Error("the departed truck's claim was tombstoned — it is on the board and a driver is on it")
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE II — a status we do not recognise
// ---------------------------------------------------------------------------

// TestAStatusWeDoNotRecogniseIsHeldReportedAndNeverTombstoned is the observed
// defect stated end to end.
//
// delivery_routes.status is VARCHAR(50) with NO CHECK CONSTRAINT (migration
// 009), so the column takes any string and the integration endpoint returns it
// verbatim; ON_HOLD was confirmed against real Postgres. Against c2cb4d9 that
// row answered false to Live() and to Dispatched(), so holds() said the board
// held nothing for the truck: the claim was classified GHOST and tombstoned with
// no wire call, no approval and no way back, the row fell out of the orphan pass
// too (it switches on the same two predicates) and was reported NOWHERE, and the
// report then said the date was in sync while the board held a route no ledger
// named.
func TestAStatusWeDoNotRecogniseIsHeldReportedAndNeverTombstoned(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.g.setBoardStatus("v1", "ON_HOLD")

	rep := d.report()
	if n := len(divergencesOfKind(rep.Divergences, DivergenceGhost)); n != 0 {
		t.Errorf("%d ghost(s) reported for a route the board is holding right now: %+v", n, rep.Divergences)
	}
	unknown := rep.divergence(t, DivergenceUnknownStatus)
	if unknown.BoardStatus != "ON_HOLD" || unknown.VehicleID != "v1" {
		t.Errorf("the report must name the row and the status nobody understood: %+v", unknown)
	}
	if unknown.RouteID == "" || unknown.StopCount == 0 || len(unknown.OrderIDs) == 0 {
		t.Errorf("a human deciding what to do needs the row itself, not just a truck id: %+v", unknown)
	}
	if rep.InSync {
		t.Error("a board carrying a status this service cannot classify must never read in_sync — the whole point is that a human has to look")
	}

	// The claim is HELD: it survives, it gates, and nothing was sent.
	if c := liveClaim(d.store.stored("plan-1"), "v1"); c == nil {
		t.Fatal("the claim on the unclassifiable route was tombstoned")
	}
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusLocked {
		t.Fatalf("a re-plan over it answered %d, want 423: %s", rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("the refused re-plan recalled %v; nothing may be sent about a row we cannot classify without an approval", got)
	}
	if c := liveClaim(d.store.stored("plan-1"), "v1"); c == nil {
		t.Fatal("the refused re-plan tombstoned the claim on the unclassifiable route — no wire call, no approval, no way back")
	}
	if !d.boardCarries("o-v1") {
		t.Fatal("the unclassifiable route was taken off the board")
	}
}

// TestAnUnrecognisedStatusOnATruckNobodyClaimsIsStillReported covers the same
// row with no ledger anywhere near it. It must not be silently reclassified as
// an orphan and offered for withdrawal: we do not know what the status means, so
// we do not know that withdrawing it is possible, let alone safe.
func TestAnUnrecognisedStatusOnATruckNobodyClaimsIsStillReported(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.boardOnly("v9", "AWAITING_PERMIT", "o-permit")

	rep := d.report()
	if rep.InSync {
		t.Error("in_sync over a row nobody can classify")
	}
	if n := len(divergencesOfKind(rep.Divergences, DivergenceOrphan)); n != 0 {
		t.Errorf("an unclassifiable row was offered as a withdrawable orphan: %+v", rep.Divergences)
	}
	got := rep.divergence(t, DivergenceUnknownStatus)
	if got.BoardStatus != "AWAITING_PERMIT" {
		t.Errorf("divergence = %+v", got)
	}
	// It is reported, and it is not a gate: a refusal nobody can clear would
	// make the date permanently un-re-plannable, which is the trap this package
	// refuses to build. See DISPATCHED and UNADDRESSABLE.
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusCreated {
		t.Fatalf("re-plan answered %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if !d.boardCarries("o-permit") {
		t.Fatal("the re-plan withdrew a route it could not classify")
	}
	// And the plan it created says the date had one on it.
	if got := repairsOfKind(d.store.stored("plan-2"), DivergenceUnknownStatus); len(got) != 1 ||
		got[0].Action != RepairReported {
		t.Errorf("the new plan must record what else was true of the date it was built over: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE III — a second row on a truck we also hold. Pin this hardest.
// ---------------------------------------------------------------------------

// TestADispatchersRunOnATruckWeAlsoClaimSurvivesAnApprovedReplan is the worst of
// the three, reproduced exactly as it was observed:
//
//	BOARD: route-2-v1 vehicle=v1 orders=[o-hand-written]   (the dispatcher's own run)
//	REPORT in_sync=true divergences=0
//	APPROVED re-plan -> 201   recalls issued: [v1 v2]
//	BOARD after: 0 routes.    dispatcher hand-written run survived: FALSE
//
// Two live rows for one truck on one day is a state the ERP genuinely produces:
// migration 009 declines a unique index on (vehicle_id, scheduled_date), and
// internal/delivery/repository.go's CreateRoute — the dealer's own dispatch UI —
// inserts with no dedup at all. newBoardView keyed live by vehicle, so the second
// row silently overwrote the first, and the orphan pass skipped any board route
// whose vehicle appeared in claimsOn(plans). The run was invisible in both
// directions at once, and the approval the dispatcher gave was for recalling OUR
// two routes; it never mentioned theirs.
func TestADispatchersRunOnATruckWeAlsoClaimSurvivesAnApprovedReplan(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.boardOnly("v1", gable.RouteStatusScheduled, "o-hand-written")

	if n := len(d.boardRows()); n != 3 {
		t.Fatalf("setup: the board must hold three rows (ours on v1 and v2, theirs on v1), got %d", n)
	}

	rep := d.report()
	if rep.InSync {
		t.Error("the report says the date is in sync while the board holds a run no plan of ours names")
	}
	orphan := rep.divergence(t, DivergenceOrphan)
	if !orphan.Contested {
		t.Error("an orphan sharing a truck with a claim of ours must be marked contested — a recall names (vehicle, date) and would take ours down with it")
	}
	if orphan.VehicleID != "v1" || len(orphan.OrderIDs) != 1 || orphan.OrderIDs[0] != "o-hand-written" {
		t.Errorf("the report must name THEIR row, not ours: %+v", orphan)
	}

	// The one that matters. An approval cannot authorize this, because the wire
	// call it would authorize cannot be aimed.
	rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an APPROVED re-plan answered %d over a run it cannot name; want 422: %s", rec.Code, rec.Body.String())
	}
	if !d.boardCarries("o-hand-written") {
		t.Fatal("an approved re-plan destroyed a run it never named — this is the harm, and no approval reaches it")
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Fatalf("the refused re-plan issued recalls %v", got)
	}
	if n := len(d.boardRows()); n != 3 {
		t.Fatalf("the board lost rows to a refused re-plan: %d of 3 left", n)
	}
	if d.store.count() != 1 {
		t.Errorf("the refused re-plan created a plan anyway: %d stored", d.store.count())
	}
	// The refusal has a remedy, and names it.
	body := rec.Body.String()
	for _, want := range []string{"v1", "cancel whichever run is wrong in GableLBM", "dispatch-board?date=2026-06-26"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal must be actionable — missing %q: %s", want, body)
		}
	}

	// And once the human does the thing the refusal asked for — cancel THEIR
	// row in GableLBM, leaving ours — the date re-plans normally. A refusal with
	// no remedy is a bug, not a safeguard, so this half is the other half of the
	// claim above.
	d.g.cancelRow(orphan.RouteID)
	rec = d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("after the duplicate is resolved the date must re-plan: %d %s", rec.Code, rec.Body.String())
	}
	if got := sorted(d.g.recalledIDs()); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("the approved re-plan recalled %v, want both of our trucks", got)
	}
	d.assertAcceptance()
}

// TestAReassignmentIsAlsoRefusedOverAContestedTruck closes the same door on the
// other writer. assignHeld recalls by (vehicle_id, scheduled_date) too, so a
// re-assignment that dropped v1 would destroy the dispatcher's run exactly as the
// re-plan did.
func TestAReassignmentIsAlsoRefusedOverAContestedTruck(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.boardOnly("v1", gable.RouteStatusScheduled, "o-hand-written")
	d.g.vehicles = d.g.vehicles[1:] // v1 would be dropped, and therefore recalled

	rec := d.assign("plan-1", `{"override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an approved re-assignment answered %d over a contested truck, want 422: %s", rec.Code, rec.Body.String())
	}
	if !d.boardCarries("o-hand-written") {
		t.Fatal("the re-assignment destroyed the dispatcher's run")
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("the refused re-assignment recalled %v", got)
	}
}

// TestASecondRowOnATruckNobodyClaimsIsAnOrdinaryOrphan keeps the refusal narrow.
// Contested is about the RECALL KEY, not about duplicates: two rows on a truck no
// plan of ours claims are both withdrawable by one (vehicle, date) recall, which
// is exactly what an approved re-plan is for. Widening the 422 to every duplicate
// would make dates un-re-plannable for a reason that has a remedy in this
// service.
func TestASecondRowOnATruckNobodyClaimsIsAnOrdinaryOrphan(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-a")
	d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-b")

	rep := d.report()
	orphans := divergencesOfKind(rep.Divergences, DivergenceOrphan)
	if len(orphans) != 2 {
		t.Fatalf("both rows must be reported, got %+v", rep.Divergences)
	}
	for _, o := range orphans {
		if o.Contested {
			t.Errorf("no claim of ours is on v9, so a recall withdraws exactly these: %+v", o)
		}
	}
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusLocked {
		t.Fatalf("want 423, got %d", rec.Code)
	}
	if rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("an approved re-plan answered %d: %s", rec.Code, rec.Body.String())
	}
	if len(d.boardRows()) != 0 {
		t.Errorf("the approval named both rows and must have withdrawn both: %+v", d.boardRows())
	}
}

// readyForPush walks a plan from ASSIGNED back to REVIEWED over the wire —
// pack, yard proof, sign-off, review — because a re-assignment rebuilds
// Plan.Loads from scratch and every one of those artifacts goes with them.
func (d *dispatchDay) readyForPush(planID string) {
	d.t.Helper()
	// The override rides on every step: the run's routes are still live on the
	// board while it is rebuilt, which is exactly the state the approval idiom
	// exists for.
	approved := `{"override":true,"approved_by":"dispatcher@dealer.com"}`
	if rec := d.post("/api/v1/workflow/plans/"+planID+"/pack", approved); rec.Code != http.StatusOK {
		d.t.Fatalf("pack: %d %s", rec.Code, rec.Body.String())
	}
	for _, l := range d.store.stored(planID).Loads {
		base := "/api/v1/workflow/plans/" + planID + "/loads/" + l.VehicleID
		if rec := d.post(base+"/proof", `{"url":"https://yard/`+l.VehicleID+`.jpg","kind":"PHOTO","added_by":"yard@dealer.com"}`); rec.Code != http.StatusOK {
			d.t.Fatalf("proof %s: %d %s", l.VehicleID, rec.Code, rec.Body.String())
		}
		if rec := d.post(base+"/sign-off", `{"signed_by":"yard@dealer.com","role":"YARD"}`); rec.Code != http.StatusOK {
			d.t.Fatalf("sign-off %s: %d %s", l.VehicleID, rec.Code, rec.Body.String())
		}
	}
	if rec := d.post("/api/v1/workflow/plans/"+planID+"/review", ``); rec.Code != http.StatusOK {
		d.t.Fatalf("review: %d %s", rec.Code, rec.Body.String())
	}
}

// TestARepushMovesTheClaimToTheNewRouteId pins the one way a correct
// implementation of route-id matching turns itself into a ghost factory.
//
// ReplaceDeliveryRoute DELETEs the prior DRAFT/SCHEDULED row and INSERTs a new
// one with a NEW id. A markRouteLive that kept the id it first recorded would
// leave every re-pushed truck claiming a row that no longer exists — a ghost the
// next re-plan tombstones, while its real route reads as an orphan. That is the
// original defect, manufactured by its own fix.
//
// Driven the way a dispatcher reaches it: push the run, re-assign it (approved),
// re-pack, re-sign, re-review, push again. The re-assignment throws the per-truck
// push acks away with Plan.Loads, so the second push really does re-send a truck
// whose claim is still live — which is exactly the revive branch of
// markRouteLive.
func TestARepushMovesTheClaimToTheNewRouteId(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	first := liveClaim(d.store.stored("plan-1"), "v1")
	if first == nil || first.RouteID == "" {
		t.Fatalf("a push must record the id GableLBM minted: %+v", first)
	}

	if rec := d.assign("plan-1", `{"override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusOK {
		t.Fatalf("re-assign: %d %s", rec.Code, rec.Body.String())
	}
	d.readyForPush("plan-1")
	d.push("plan-1")

	second := liveClaim(d.store.stored("plan-1"), "v1")
	if second == nil {
		t.Fatal("the re-push lost the claim")
	}
	if second.RouteID == first.RouteID {
		t.Fatalf("the claim still names the row the replace DELETEd (%s) — it would read as a ghost and its real route as an orphan", first.RouteID)
	}
	if rep := d.report(); !rep.InSync {
		t.Errorf("a plain re-push left the date out of sync: %+v", rep.Divergences)
	}
	d.assertAcceptance()
}

// TestAPushThatDisplacesAnotherPlansRouteRecordsTheNewId is the same rule across
// two plans, which is where it actually bites: GableLBM's replace destroys the
// FIRST plan's row and mints a fresh id for the second. A push that recorded
// anything but that id would leave the pusher claiming a row that never existed
// while its real route read as a run nobody named.
func TestAPushThatDisplacesAnotherPlansRouteRecordsTheNewId(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v3"))
	d.push("plan-1")
	before := liveClaim(d.store.stored("plan-1"), "v1")
	d.push("plan-2")

	after := liveClaim(d.store.stored("plan-2"), "v1")
	if after == nil || after.RouteID == "" || after.RouteID == before.RouteID {
		t.Fatalf("the displacing push must record the id the ERP minted for ITS row: before=%+v after=%+v", before, after)
	}
	if c := liveClaim(d.store.stored("plan-1"), "v1"); c != nil {
		t.Errorf("the displaced plan still claims v1: %+v", c)
	}
	if rep := d.report(); !rep.InSync {
		t.Errorf("two plans, one truck, both pushed — and the date must still reconcile: %+v", rep.Divergences)
	}
	d.assertAcceptance()
}

// TestAClaimWrittenBeforeRouteIdsIsNeitherGhostNorOrphan is the migration
// question, and it is answered without a migration.
//
// Every LiveRoute already in workflow_plans carries no route id, and there is no
// backfill that could invent one — GableLBM's board is the only place those ids
// exist and nothing correlates them to a plan after the fact. So an id-less claim
// is matched to the board by TRUCK, exactly as the whole ledger used to be, and
// it is matched in a SECOND pass so an id-bearing claim always gets its own row
// first. It therefore cannot become a ghost (a row for its truck backs it) and
// cannot leave its own row looking like an orphan.
func TestAClaimWrittenBeforeRouteIdsIsNeitherGhostNorOrphan(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")

	// Age the ledger back to what a pre-change row looks like on disk.
	d.store.mu.Lock()
	for i := range d.store.plans["plan-1"].LiveRoutes {
		d.store.plans["plan-1"].LiveRoutes[i].RouteID = ""
	}
	d.store.mu.Unlock()

	rep := d.report()
	if !rep.InSync {
		t.Fatalf("a ledger written before route ids existed reads as divergent: %+v", rep.Divergences)
	}
	// It still gates, and an approved re-plan still recalls what it names.
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusLocked {
		t.Fatalf("want 423, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("approved re-plan: %d %s", rec.Code, rec.Body.String())
	}
	if got := sorted(d.g.recalledIDs()); !equalStrings(got, []string{"v1", "v2"}) {
		t.Errorf("recalled %v, want both trucks", got)
	}
	d.assertAcceptance()
}

// TestALegacyClaimStillCannotHideASecondRowOnItsTruck is the half of the same
// question that decides whether the defect is actually closed for existing data.
//
// An id-less claim consumes ONE row, not a truck. If it absorbed the truck, every
// ledger written before this change would go on hiding a dispatcher's hand-built
// run for exactly as long as it lived — which for a pushed plan is the rest of
// the day it matters on.
func TestALegacyClaimStillCannotHideASecondRowOnItsTruck(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.store.mu.Lock()
	for i := range d.store.plans["plan-1"].LiveRoutes {
		d.store.plans["plan-1"].LiveRoutes[i].RouteID = ""
	}
	d.store.mu.Unlock()

	d.boardOnly("v1", gable.RouteStatusScheduled, "o-hand-written")

	orphan := d.report().divergence(t, DivergenceOrphan)
	if !orphan.Contested || orphan.VehicleID != "v1" {
		t.Fatalf("the second row on v1 is invisible to a legacy claim: %+v", orphan)
	}
	if rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an approved re-plan answered %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !d.boardCarries("o-hand-written") {
		t.Fatal("a pre-existing ledger still let an approved re-plan destroy a run it never named")
	}
}

// TestAClaimNamingARouteTheBoardReplacedIsAGhostAndTheNewRowIsSeen is the case
// route-id matching exists to tell apart, and the case a truck-keyed match got
// exactly backwards.
//
// Our route on v1 was cancelled in GableLBM and a different run put in its place.
// The claim names a row that is gone — a ghost, repaired without asking, because
// repairing it takes nothing off anybody's board. The new row is a run nobody
// named — an orphan, never touched without an approval. Matched by truck, the two
// cancelled out: the claim looked backed and the row looked accounted for.
func TestAClaimNamingARouteTheBoardReplacedIsAGhostAndTheNewRowIsSeen(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.g.removeFromBoard("v1", d.date)
	d.boardOnly("v1", gable.RouteStatusScheduled, "o-hand-written")

	rep := d.report()
	ghost := rep.divergence(t, DivergenceGhost)
	if ghost.VehicleID != "v1" {
		t.Errorf("ghost = %+v", ghost)
	}
	orphan := rep.divergence(t, DivergenceOrphan)
	if orphan.VehicleID != "v1" || orphan.Contested {
		t.Errorf("the replacement row is an ordinary orphan — the only claim on v1 is a ghost, so a recall withdraws exactly it: %+v", orphan)
	}
	if rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("approved re-plan: %d %s", rec.Code, rec.Body.String())
	}
	d.assertAcceptance()
}

// TestBothRowsOnOneUnclaimedTruckAreReportedInAFixedOrder pins the tiebreak the
// report's determinism now rests on.
//
// A truck could not carry two rows before this change, so ordering divergences by
// (kind, vehicle, plan) was total. It no longer is: two orphans on one truck tie
// on all three, and their order would then follow the ERP's scan — which makes
// every refusal sentence and every JSON body built from the report unassertable,
// and makes a UI list reshuffle itself between polls.
func TestBothRowsOnOneUnclaimedTruckAreReportedInAFixedOrder(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	for _, o := range []string{"o-hand-a", "o-hand-b", "o-hand-c"} {
		d.boardOnly("v9", gable.RouteStatusScheduled, o)
	}

	// The fixture only proves anything if the ERP's own scan order DISAGREES
	// with route-id order somewhere; otherwise a reconciler that simply
	// preserved the scan would look sorted. delivery_routes.id is a v4 UUID
	// upstream and the double mints ids with no order information for exactly
	// this reason.
	rows := d.boardRows()
	inverted := false
	for i := 1; i < len(rows); i++ {
		if rows[i].RouteID < rows[i-1].RouteID {
			inverted = true
		}
	}
	if !inverted {
		t.Fatalf("fixture is useless: the board already hands these back in route-id order: %+v", rows)
	}

	orphans := divergencesOfKind(d.report().Divergences, DivergenceOrphan)
	if len(orphans) != 3 {
		t.Fatalf("want all three rows, got %+v", orphans)
	}
	for i := 1; i < len(orphans); i++ {
		if orphans[i].RouteID <= orphans[i-1].RouteID {
			t.Fatalf("the orphans came back in the ERP's scan order (%q then %q) — they tie on kind, vehicle and plan, so without a route-id tiebreak every sentence and JSON body built from this report is unassertable",
				orphans[i-1].RouteID, orphans[i].RouteID)
		}
	}
	// And re-reading gives the identical order.
	for i, again := range divergencesOfKind(d.report().Divergences, DivergenceOrphan) {
		if again.RouteID != orphans[i].RouteID {
			t.Fatalf("the report reordered itself between polls at %d: %q then %q", i, orphans[i].RouteID, again.RouteID)
		}
	}
}

// TestARouteOfOursThatLosesItsTruckIsReportedRatherThanAccountedFor covers the
// nullable column on a row we DO name.
//
// delivery_routes.vehicle_id is nullable, so a route this service wrote can lose
// its truck upstream. The claim is not a ghost — the row is right there — but the
// recall key is (vehicle_id, scheduled_date) and there is now no message this
// service could send that names it. Treating it as merely "accounted for" would
// promise, in an approval prompt, to withdraw a route that cannot be addressed.
func TestARouteOfOursThatLosesItsTruckIsReportedRatherThanAccountedFor(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	claim := liveClaim(d.store.stored("plan-1"), "v1")
	if claim == nil || claim.RouteID == "" {
		t.Fatalf("setup: %+v", claim)
	}
	d.g.unassignVehicle(claim.RouteID)

	rep := d.report()
	if n := len(divergencesOfKind(rep.Divergences, DivergenceGhost)); n != 0 {
		t.Errorf("the row is on the board, so the claim on it is not a ghost: %+v", rep.Divergences)
	}
	got := rep.divergence(t, DivergenceUnaddressable)
	if got.RouteID != claim.RouteID {
		t.Errorf("the report must name the row that lost its truck: %+v", got)
	}
	if rep.InSync {
		t.Error("a board row nothing can address must not read as in sync")
	}
	if c := liveClaim(d.store.stored("plan-1"), "v1"); c == nil {
		t.Error("the claim was tombstoned for a route that is still on the board")
	}
}

// TestAReassignmentIsNotMadeToBegForRoutesThatDoNotExist is the other direction
// of the same defect, and the one that would have made the whole change
// self-defeating.
//
// A gate reading the raw ledger over-refuses as readily as it under-refuses: a
// plan whose claims the board does not back strands NOTHING, and demanding an
// approval for it asks a dispatcher to authorize cancelling runs that are not
// there — and the recall that approval buys is keyed (vehicle, date), so
// exercising it cancels whatever those trucks acquire next. The cheap pre-claim
// ask has to discount ghosts too, or it refuses before the repair that would
// have cleared them is ever reached.
func TestAReassignmentIsNotMadeToBegForRoutesThatDoNotExist(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	// A push that died on truck 2: v1 is live, the plan stays REVIEWED.
	d.g.pushErrAfter = 1
	d.post("/api/v1/workflow/plans/plan-1/push", ``)
	d.g.pushErrAfter = 0
	if liveClaim(d.store.stored("plan-1"), "v1") == nil {
		t.Fatal("setup: the partial push must have left a live claim on v1")
	}
	// GableLBM's own UI cancels it. Our ledger goes on claiming it.
	d.g.removeFromBoard("v1", d.date)

	rec := d.assign("plan-1", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a re-assignment answered %d over a ledger of pure ghosts — there is nothing on the dealer's board to strand and nothing for an approver to weigh: %s",
			rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("it recalled %v on the strength of a claim the board does not back", got)
	}
	if c := liveClaim(d.store.stored("plan-1"), "v1"); c != nil {
		t.Errorf("the stale claim survived the re-assignment: %+v", c)
	}
	d.assertAcceptance()
}
