// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// The dispatch board is the authority, proved through the real handlers.
//
// Every test in this file is about a state that USED TO BE INVISIBLE. The
// previous gate read Plan.LiveRoutes and nothing else, and Plan.LiveRoutes is a
// cache of GableLBM's board that diverges from it on any crash between the ERP
// write and the ledger write. So the two failures below were both reachable
// with no concurrency at all:
//
//   - a route live on the board that no plan names — a re-ingest planned
//     straight over it, and the new plan had no idea the truck was taken;
//   - a ledger claiming a truck the board does not hold — every re-plan of that
//     date demanded an approval for a route that did not exist, and the recall
//     it authorized was keyed (vehicle, date), so it would have cancelled
//     whatever route that truck acquired next.
//
// The two are NOT symmetric and this file exists mostly to pin that. The ghost
// is ours to fix and is fixed automatically. The orphan is the dealer's and is
// never touched without a named approval.

// errUpstreamDown stands in for a GableLBM that cannot be reached — the network
// failure, not a refusal, because a refusal would already be a 4xx the client
// carries meaning for.
var errUpstreamDown = errors.New("call GET /api/integration/delivery-routes: dial tcp: connect: connection refused")

// ---------------------------------------------------------------------------
// harness additions
// ---------------------------------------------------------------------------

// get drives a read endpoint over the wire, like post does for writes.
func (d *dispatchDay) get(path string) *httptest.ResponseRecorder {
	d.t.Helper()
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// boardOnly puts a route on the dealer's board that this service did not write
// — a dispatcher building a run by hand in GableLBM, or the ERP half of a push
// whose ledger write was lost.
//
// It lands in the SAME board the service's own pushes go to, so the acceptance
// oracle sees it too. That matters: an orphan IS an acceptance-II violation,
// and what this change does is turn it from invisible into refused.
func (d *dispatchDay) boardOnly(vehicleID, status string, orderIDs ...string) {
	d.t.Helper()
	d.g.pushOutside(vehicleID, d.date, status, orderIDs...)
}

// report reads the reconciliation endpoint and decodes it.
func (d *dispatchDay) report() BoardReport {
	d.t.Helper()
	rec := d.get("/api/v1/workflow/dispatch-board?date=" + d.date)
	if rec.Code != http.StatusOK {
		d.t.Fatalf("dispatch-board report answered %d: %s", rec.Code, rec.Body.String())
	}
	var out BoardReport
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		d.t.Fatalf("decode dispatch-board report: %v (%s)", err, rec.Body.String())
	}
	return out
}

// divergence returns the single divergence of a kind, failing if there is not
// exactly one. "Exactly one" is the assertion: a reconciler that reported the
// same truck under two kinds would be describing a decision it has not made.
func (r BoardReport) divergence(t *testing.T, kind string) BoardDivergence {
	t.Helper()
	got := divergencesOfKind(r.Divergences, kind)
	if len(got) != 1 {
		t.Fatalf("want exactly one %s divergence, got %d: %+v", kind, len(got), r.Divergences)
	}
	return got[0]
}

// repairsOfKind pulls what a plan recorded doing about one kind of divergence.
func repairsOfKind(p *Plan, kind string) []BoardRepair {
	out := []BoardRepair{}
	for _, r := range p.BoardRepairs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// ACCEPTANCE I — a route only the board knows is SEEN by the re-plan gate
// ---------------------------------------------------------------------------

// TestAReplanCannotPlanOverARouteOnlyTheBoardKnows is the whole point of this
// change, stated as two HTTP calls.
//
// A truck holds a live route on the dealer's board for this date and NO plan's
// ledger names it. That is the exact state a crash between the ERP write and
// the ledger write leaves behind, and it is also what a dispatcher creating a
// run by hand in GableLBM looks like. Against the ledger-only gate this
// re-ingest answered 201: every ledger was empty, so the gate saw a free day
// and planned over a truck that was already committed.
//
// It now answers 423, and the refusal NAMES the truck — because approving it
// means cancelling a run somebody may be about to drive, and "some route
// somewhere is in the way" is not something a person can act on.
func TestAReplanCannotPlanOverARouteOnlyTheBoardKnows(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	// Nothing is pushed: every ledger for this date is empty, which is exactly
	// what made this invisible.
	d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-written")

	if claims := d.claims(); len(claims) != 0 {
		t.Fatalf("setup: no ledger may name anything, got %v", claims)
	}
	planned, writes := d.store.count(), d.store.updates

	rec := d.ingest(`{"date":"2026-06-26"}`)

	if rec.Code != http.StatusLocked {
		t.Fatalf("re-planning over a route only the board knows answered %d, want 423 — this is the harm the whole branch exists to prevent: %s",
			rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"v9", "manual approval", "NO plan of ours names"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal must contain %q so the approver knows what they would be cancelling: %s", want, body)
		}
	}
	if n := d.store.count(); n != planned {
		t.Errorf("the store holds %d plans, want %d — a refused re-plan creates nothing", n, planned)
	}
	if d.store.updates != writes {
		t.Errorf("a refused re-plan wrote %d time(s); it must write nothing", d.store.updates-writes)
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("a REFUSED re-plan recalled %v — nothing may come off the board without an approval", got)
	}
	if got := d.g.pushedIDs(); len(got) != 1 || got[0] != "v9" {
		t.Errorf("the board must be left exactly as it was, holds %v", got)
	}
	// And the same truck is discoverable without attempting a re-plan at all.
	div := d.report().divergence(t, DivergenceOrphan)
	if div.VehicleID != "v9" || div.StopCount != 1 || len(div.OrderIDs) != 1 || div.OrderIDs[0] != "o-hand-written" {
		t.Errorf("the report must carry what is ON the truck, not just its id: %+v", div)
	}
}

// TestAnApprovedReplanWithdrawsTheRouteNoPlanNamed is the other half: the
// refusal above has a remedy, and exercising it converges the date.
//
// A gate whose refusal cannot be cleared is a bug, not a safeguard — the date
// would be permanently un-re-plannable — so the approval has to actually take
// the unnamed route off the board, and has to be recorded where an incident
// review can find it. There is no superseded plan here to record it on, which
// is exactly the case a ledger-shaped audit trail cannot represent.
func TestAnApprovedReplanWithdrawsTheRouteNoPlanNamed(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-written")

	rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("an approved re-plan must proceed: %d %s", rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 1 || got[0] != "v9" {
		t.Fatalf("recalled %v, want exactly [v9] — the approval named that truck and nothing else", got)
	}
	if got := d.g.pushedIDs(); len(got) != 0 {
		t.Errorf("the board still holds %v after an approved re-plan", got)
	}

	created := d.store.stored("plan-2")
	repairs := repairsOfKind(created, DivergenceOrphan)
	if len(repairs) != 1 {
		t.Fatalf("the new plan must record what it withdrew: %+v", created.BoardRepairs)
	}
	r := repairs[0]
	if r.Action != RepairRecalled || r.VehicleID != "v9" || r.By != "dispatcher@dealer.com" || r.Note == "" {
		t.Errorf("the repair record must name the act, the truck, the approver and the reason: %+v", r)
	}
	if len(created.PushedOverrides) != 1 {
		t.Fatalf("the approval must be recorded on the only plan that exists to record it on: %+v", created.PushedOverrides)
	}
	if ov := created.PushedOverrides[0]; ov.Action != actionSupersede || ov.ApprovedBy != "dispatcher@dealer.com" || !strings.Contains(ov.Note, "v9") {
		t.Errorf("override %+v must name the action, the approver and the truck", ov)
	}
	// The date is now genuinely in sync, which is what makes the refusal above
	// a gate rather than a dead end.
	if rep := d.report(); !rep.InSync {
		t.Errorf("after the approved re-plan the date must reconcile clean: %+v", rep.Divergences)
	}
	d.assertAcceptance()
}

// TestAnOrphanThatAppearsBetweenTheTwoReadsIsStillSeen is why the DECIDING gate
// is given the orphans too, and not just the cheap one.
//
// The window is real and is not closed by the dispatch-date claim. The first
// board read is taken deliberately outside the claim, and between it and the
// claim being granted sit three ERP round-trips: a push landing in that window
// and then failing to record its ledger leaves a live route no plan names, and
// a dispatcher building a run by hand in GableLBM does the same. The cheap ask
// saw a clean date; the re-plan is about to be built over a truck that is
// taken.
//
// Feeding the second gate an empty orphan set is invisible to every other test
// in this file, because the first gate catches the ordinary case. This is the
// one that fails.
func TestAnOrphanThatAppearsBetweenTheTwoReadsIsStillSeen(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	// The order pull is the window: it runs AFTER the cheap ask has already
	// answered on a clean board and BEFORE the dispatch-date claim is granted.
	// Landing the route in the board read itself would be seen by the cheap ask
	// and would prove nothing about the second one.
	d.g.onListOrders = func() {
		d.g.pushOutside("v9", d.date, gable.RouteStatusScheduled, "o-late")
	}

	rec := d.ingest(`{"date":"2026-06-26"}`)
	if rec.Code != http.StatusLocked {
		t.Fatalf("a route that appeared after the cheap ask answered %d, want 423 — the deciding gate is not being shown the board it just read: %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "v9") {
		t.Errorf("the refusal must name the truck that appeared: %s", rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("recalled %v without an approval", got)
	}
	if n := d.store.count(); n != 1 {
		t.Errorf("the store holds %d plans, want 1 — the refused re-plan created nothing", n)
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE II — a ghost claim is repaired automatically, and idempotently
// ---------------------------------------------------------------------------

// TestAGhostClaimIsRepairedAutomaticallyAndIsIdempotent covers the safe half of
// the asymmetry.
//
// plan-1's routes really were on the board; a dispatcher then cancelled them in
// GableLBM and told this service nothing. Our ledger goes on claiming two
// trucks that the system of record does not hold.
//
// Re-planning that date must be FRICTIONLESS. Not approved-then-recalled —
// frictionless, with no 423 and no wire call — because there is nothing on
// anybody's board to withdraw and demanding an approval for it would train a
// dispatcher to click through the one prompt that also guards real routes.
//
// The second ingest asserts idempotence the only way that means anything: by
// counting WRITES. An implementation that re-tombstoned already-tombstoned
// entries would pass every state assertion here and bump a version on every
// re-plan for the rest of the day.
func TestAGhostClaimIsRepairedAutomaticallyAndIsIdempotent(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	if got := d.g.pushedIDs(); len(got) != 2 {
		t.Fatalf("setup: the board holds %v, want both trucks", got)
	}
	// The dealer cancels both runs in their own UI. Nothing reaches us.
	d.g.removeFromBoard("v1", d.date)
	d.g.removeFromBoard("v2", d.date)
	if claims := d.claims(); len(claims) != 2 {
		t.Fatalf("setup: the ledger must still claim both trucks, got %v", claims)
	}

	// Reading the report over a date that HAS ghosts must not repair them. A
	// GET that writes cannot be polled or put on a dashboard, and it would
	// change a dispatcher's state for asking a question.
	before := d.store.updates
	if rep := d.report(); len(divergencesOfKind(rep.Divergences, DivergenceGhost)) != 2 {
		t.Fatalf("the report must NAME the stale claims: %+v", rep.Divergences)
	}
	if d.store.updates != before {
		t.Errorf("reading the report repaired %d ledger(s) — the report reports, the re-plan repairs", d.store.updates-before)
	}
	if n := len(liveRoutes(d.store.stored("plan-1"))); n != 2 {
		t.Errorf("the report tombstoned %d claim(s) on plan-1; it must write nothing at all", 2-n)
	}

	rec := d.ingest(`{"date":"2026-06-26"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-planning a date whose claims are all stale must be frictionless, got %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("repairing a ghost sent %v to GableLBM — there is nothing upstream to withdraw, and a wire call here would cancel whatever route that truck acquires next", got)
	}

	old := d.store.stored("plan-1")
	if n := len(liveRoutes(old)); n != 0 {
		t.Errorf("plan-1 still claims %d live route(s) after the repair", n)
	}
	if len(old.LiveRoutes) != 2 {
		t.Fatalf("a repair tombstones, it never deletes: %+v", old.LiveRoutes)
	}
	for _, r := range old.LiveRoutes {
		if r.RecalledBy != systemRecaller {
			t.Errorf("a correction nobody asked for must say so, not read as an approval: %+v", r)
		}
		if !strings.Contains(r.RecallNote, "system of record") {
			t.Errorf("the tombstone must say WHY the claim was dropped: %+v", r)
		}
	}
	repairs := repairsOfKind(d.store.stored("plan-2"), DivergenceGhost)
	if len(repairs) != 2 {
		t.Errorf("the new plan must record that the date it was built on had %d stale claim(s), got %+v", 2, repairs)
	} else if repairs[0].Action != RepairTombstoned {
		t.Errorf("a ghost is TOMBSTONED, not recalled: %+v", repairs[0])
	}

	// --- idempotence ---
	writes := d.store.updates
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the second re-plan must also be frictionless: %d %s", rec.Code, rec.Body.String())
	}
	if d.store.updates != writes {
		t.Errorf("the second re-plan wrote %d time(s) repairing ghosts that were already repaired; a repair that keeps writing is not idempotent",
			d.store.updates-writes)
	}
	if again := d.store.stored("plan-1"); !equalTombstones(old, again) {
		t.Errorf("the second re-plan rewrote plan-1's tombstones:\n before %+v\n after  %+v", old.LiveRoutes, again.LiveRoutes)
	}
}

// TestAGhostRepairLeavesARealClaimStanding is the mixed case, and it is the one
// that catches a repair that corrects the ledger and then cannot write to it
// again.
//
// plan-1 holds two trucks; only one is cancelled upstream. The repair must
// tombstone that one, leave the other alone, and the re-plan must then still be
// able to supersede plan-1 for the truck that IS live — which means the repair
// has to hand back a plan whose version is current. A repair that mirrored the
// tombstone onto its stale in-memory copy would pass every assertion about the
// ledger and then fail the very next write with a 409 the dispatcher cannot act
// on.
func TestAGhostRepairLeavesARealClaimStanding(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.g.removeFromBoard("v1", d.date) // only v1 is cancelled upstream

	// v2 is genuinely live, so this re-plan still needs an approval — for v2
	// alone.
	rec := d.ingest(`{"date":"2026-06-26"}`)
	if rec.Code != http.StatusLocked {
		t.Fatalf("a date with one real live route must still require approval, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "Flatbed 1") {
		t.Errorf("the refusal names a truck whose route the board does not hold — the ghost was not repaired before the gate ran: %s", body)
	}
	if !strings.Contains(rec.Body.String(), "Flatbed 2") {
		t.Errorf("the refusal must name the truck that IS live: %s", rec.Body.String())
	}

	rec = d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the approved re-plan must proceed, got %d: %s — a repair that leaves a stale version turns into a 409 here",
			rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 1 || got[0] != "v2" {
		t.Errorf("recalled %v, want exactly [v2]: the ghost had nothing to withdraw and the live route did", got)
	}
	old := d.store.stored("plan-1")
	if n := len(liveRoutes(old)); n != 0 {
		t.Errorf("plan-1 must hold nothing live after the approved re-plan, got %d", n)
	}
	byVehicle := map[string]LiveRoute{}
	for _, r := range old.LiveRoutes {
		byVehicle[r.VehicleID] = r
	}
	if byVehicle["v1"].RecalledBy != systemRecaller {
		t.Errorf("v1 was a ghost and must be attributed to the system, not the approver: %+v", byVehicle["v1"])
	}
	if byVehicle["v2"].RecalledBy != "dispatcher@dealer.com" {
		t.Errorf("v2 was a real recall and must be attributed to the approver: %+v", byVehicle["v2"])
	}
	d.assertAcceptance()
}

// TestARepairNeverRewritesAClosedChapter pins the one invariant the ledger
// correction shares with every recall path in this package.
//
// A tombstone records WHO took a route off the board and why. A correction that
// rewrote an entry it found already closed would overwrite a dispatcher's
// approved recall with "gable-ai-lm", and the answer to "who cancelled this
// customer's delivery?" would silently become "the system did". Both callers of
// this primitive select only live entries, so the guard is unreachable by
// construction today — which is exactly why it needs a test of its own: the
// next caller will not be, and nothing else in the suite would notice.
func TestARepairNeverRewritesAClosedChapter(t *testing.T) {
	closed := timePtr()
	store := newFakePlanStore(&Plan{
		ID: "plan-1", PlanDate: "2026-06-26", Status: StatusPushed,
		LiveRoutes: []LiveRoute{
			{VehicleID: "v1", VehicleName: "Flatbed 1", PushedAt: *closed,
				RecalledAt: closed, RecalledBy: "dispatcher@dealer.com", RecallNote: "dropped by re-assigning trucks"},
			{VehicleID: "v2", VehicleName: "Flatbed 2", PushedAt: *closed},
		},
	})
	svc := newTestService(store, nil, Config{})

	fresh, n, err := svc.tombstoneVehicles(t.Context(), "plan-1",
		map[string]bool{"v1": true, "v2": true}, time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC), systemRecaller, "tombstoned: not on the board")
	if err != nil {
		t.Fatalf("tombstoneVehicles: %v", err)
	}
	if n != 1 {
		t.Errorf("closed %d entries, want 1 — v1 was already closed and must not be counted again", n)
	}
	got := map[string]LiveRoute{}
	for _, r := range store.stored("plan-1").LiveRoutes {
		got[r.VehicleID] = r
	}
	if got["v1"].RecalledBy != "dispatcher@dealer.com" || got["v1"].RecallNote != "dropped by re-assigning trucks" {
		t.Errorf("an already-closed tombstone was rewritten, erasing who approved it: %+v", got["v1"])
	}
	if !got["v1"].RecalledAt.Equal(*closed) {
		t.Errorf("an already-closed tombstone had its timestamp moved: %+v", got["v1"])
	}
	if got["v2"].RecalledBy != systemRecaller {
		t.Errorf("the live entry was not corrected: %+v", got["v2"])
	}
	// The caller must be handed the PERSISTED plan, or its next version-checked
	// write turns a successful repair into a 409.
	if fresh == nil || fresh.Version != store.stored("plan-1").Version {
		t.Errorf("tombstoneVehicles returned a plan at version %v, store holds %d", fresh, store.stored("plan-1").Version)
	}
}

// equalTombstones compares two snapshots of one plan's ledger by the fields a
// repair writes, so "did the second run touch it?" is answerable.
func equalTombstones(a, b *Plan) bool {
	if len(a.LiveRoutes) != len(b.LiveRoutes) {
		return false
	}
	for i := range a.LiveRoutes {
		x, y := a.LiveRoutes[i], b.LiveRoutes[i]
		switch {
		case x.VehicleID != y.VehicleID, x.RecalledBy != y.RecalledBy, x.RecallNote != y.RecallNote:
			return false
		case (x.RecalledAt == nil) != (y.RecalledAt == nil):
			return false
		case x.RecalledAt != nil && !x.RecalledAt.Equal(*y.RecalledAt):
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// ACCEPTANCE III — an orphan is NEVER silently recalled
// ---------------------------------------------------------------------------

// TestAnOrphanIsNeverSilentlyRecalled walks every path that observes an orphan
// and asserts that not one of them withdraws it.
//
// This is the requirement that says what the change must NOT do, and it is the
// easy one to lose: once the reconciler can see an orphan, cancelling it is one
// line away and would make every other test in this file greener. A truck may
// be about to drive that run.
func TestAnOrphanIsNeverSilentlyRecalled(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-written")
	writes := d.store.updates

	// 1. Reading the report — twice, because a "repair on read" would be
	//    invisible on a single call.
	for i := 0; i < 2; i++ {
		if rep := d.report(); len(divergencesOfKind(rep.Divergences, DivergenceOrphan)) != 1 {
			t.Fatalf("read %d: the report must name the orphan: %+v", i, rep.Divergences)
		}
	}
	if d.store.updates != writes {
		t.Errorf("reading the dispatch-board report wrote %d time(s) — a GET that mutates cannot be polled", d.store.updates-writes)
	}

	// 2. A re-plan refused for want of an approval.
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusLocked {
		t.Fatalf("want 423, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. A re-plan of a DIFFERENT date, which must not touch this one at all.
	if rec := d.ingest(`{"date":"2026-06-27"}`); rec.Code != http.StatusCreated {
		t.Fatalf("re-planning another date must be unaffected: %d %s", rec.Code, rec.Body.String())
	}

	// 4. Pushing the plan that exists, which writes the board for other trucks.
	d.push("plan-1")

	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Fatalf("something withdrew %v without an approval — an orphan is surfaced, never silently recalled", got)
	}
	if board := d.board(); !board["v9"] {
		t.Errorf("the orphan is gone from the board and nobody approved it: %v", board)
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE IV — a departed truck is never treated as reclaimable
// ---------------------------------------------------------------------------

// TestATruckInTransitIsNeverTreatedAsReclaimable pins the boundary of the gate.
//
// A truck that has left the yard holds a route no plan of ours names. It is
// tempting to treat that as an orphan — it looks identical from the ledger's
// side — and doing so would be a serious defect in BOTH directions:
//
//   - refusing the re-plan would be a refusal with no remedy, because GableLBM
//     answers every recall of a departed route with a terminal 409. The date
//     would be permanently un-re-plannable by any action a dispatcher can take.
//   - approving it would propose to cancel a run that is physically happening.
//
// So it is reported as DISPATCHED, it does not gate, and it is never sent to
// the recall endpoint. The double refuses that recall with the real 409, so a
// misclassification fails here rather than passing quietly.
func TestATruckInTransitIsNeverTreatedAsReclaimable(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.boardOnly("v9", gable.RouteStatusInTransit, "o-on-the-road")

	rec := d.ingest(`{"date":"2026-06-26"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a departed truck must not gate a re-plan (no approval could ever clear it), got %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Fatalf("recalled %v — a truck that has left the yard is history, not inventory", got)
	}

	rep := d.report()
	div := rep.divergence(t, DivergenceDispatched)
	if div.VehicleID != "v9" || div.BoardStatus != gable.RouteStatusInTransit {
		t.Errorf("the departed truck must be reported as DISPATCHED with its real status: %+v", div)
	}
	if n := len(divergencesOfKind(rep.Divergences, DivergenceOrphan)); n != 0 {
		t.Fatalf("a departed truck reported as an ORPHAN would be proposed for cancellation: %+v", rep.Divergences)
	}
	repairs := repairsOfKind(d.store.stored("plan-2"), DivergenceDispatched)
	if len(repairs) != 1 || repairs[0].Action != RepairReported {
		t.Errorf("the new plan must record that it was built over a truck already out, and that nothing was done to it: %+v", repairs)
	}

	// And an approved re-plan does not reach for it either: the override
	// authorizes withdrawing what is reclaimable, and this is not.
	if rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("an approved re-plan over a departed truck must proceed: %d %s", rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("an approval must not be readable as consent to cancel a run that is physically happening: recalled %v", got)
	}
}

// TestALedgerClaimOnADepartedTruckIsNotAGhost is the same boundary from the
// ledger's side, and it is the one a naive "is it on the live board?" test gets
// wrong.
//
// plan-1 pushed v1 and v1 has since departed. The board's live set no longer
// contains it, so a repair keyed on live-only would tombstone the claim — this
// service writing down "we took this off the board" about a truck that is at
// that moment driving the run, and erasing the only record that the route is
// ours. The claim is real. It must survive, and the re-plan must still be
// gated by it.
func TestALedgerClaimOnADepartedTruckIsNotAGhost(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"))
	d.push("plan-1")
	d.g.setBoardStatus("v1", gable.RouteStatusInTransit)

	rec := d.ingest(`{"date":"2026-06-26"}`)
	if rec.Code != http.StatusLocked {
		t.Fatalf("a claim on a departed truck is REAL and must still gate the date, got %d: %s",
			rec.Code, rec.Body.String())
	}
	if p := d.store.stored("plan-1"); len(liveRoutes(p)) != 1 {
		t.Fatalf("the claim was tombstoned as a ghost while the truck is driving the run: %+v", p.LiveRoutes)
	}
	if n := len(divergencesOfKind(d.report().Divergences, DivergenceGhost)); n != 0 {
		t.Errorf("a departed truck our ledger names is not a divergence at all: %+v", d.report().Divergences)
	}

	// Approving cannot clear it either — GableLBM refuses the recall — and the
	// dispatcher is told what to do instead of being looped.
	rec = d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("recalling a departed route is terminal, want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "call the driver") {
		t.Errorf("the refusal must tell the dispatcher the one thing that can still be done: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE VI — an unreachable ERP fails CLOSED
// ---------------------------------------------------------------------------

// TestAReplanIsRefusedWhenTheDispatchBoardCannotBeRead pins the decision, which
// is the one place this change could quietly undo itself.
//
// Falling back to the ledger when the board cannot be read would be the whole
// harm class restored on a network blip — and worse, restored exactly when a
// flaky ERP has most likely just left a half-written push behind. So the
// re-plan is refused, and the refusal is 502 rather than the 423 approval
// prompt: an approval means "yes, I accept the consequence you described", and
// here the consequence is unknown, which is what unreadable means. An override
// must therefore buy nothing.
func TestAReplanIsRefusedWhenTheDispatchBoardCannotBeRead(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.g.boardErr = errUpstreamDown
	planned, writes := d.store.count(), d.store.updates

	for _, body := range []string{
		`{"date":"2026-06-26"}`,
		`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`,
	} {
		rec := d.ingest(body)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("an unreadable board must refuse the re-plan, got %d for %s: %s", rec.Code, body, rec.Body.String())
		}
		if msg := rec.Body.String(); !strings.Contains(msg, "dispatch board") {
			t.Errorf("the refusal must say WHAT could not be read, not just \"ingest failed\": %s", msg)
		}
	}
	if n := d.store.count(); n != planned {
		t.Errorf("the store holds %d plans, want %d — nothing is created over a board that cannot be seen", n, planned)
	}
	if d.store.updates != writes {
		t.Errorf("a refused re-plan wrote %d time(s)", d.store.updates-writes)
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Errorf("recalled %v while blind", got)
	}
	// The report answers the same way, rather than reporting a clean date.
	if rec := d.get("/api/v1/workflow/dispatch-board?date=2026-06-26"); rec.Code != http.StatusBadGateway {
		t.Errorf("the report must not answer 200 with an empty board when nothing is known: %d %s", rec.Code, rec.Body.String())
	}

	// It is a refusal, not a latch: the moment the ERP answers, the same call
	// proceeds. A fail-closed rule that needed a restart to clear would be its
	// own outage.
	d.g.boardErr = nil
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusCreated {
		t.Fatalf("once the board is readable again the re-plan must proceed: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// where the board read sits
// ---------------------------------------------------------------------------

// TestTheBoardIsReadTwiceAndTheDecidingReadIsUnderTheDateClaim pins the
// PLACEMENT, which no end-state assertion can see.
//
// Four claims, each of which a plausible alternative implementation breaks:
//
//   - the board is read TWICE per re-plan, mirroring the two ledger asks. One
//     read is not enough in either position. Read it only under the claim and
//     the cheap ask upstream still refuses on ghosts — which would make the
//     ghost repair unreachable, because the re-plan that performs it is
//     rejected before it starts. Read it only outside the claim and the recall
//     set is computed from a board another writer can still be moving.
//   - the DECIDING read is under the claim. Proved by landing a competing
//     writer for the same date inside the second read and watching it be
//     refused with 409.
//   - the FIRST read is NOT under the claim, which is what lets it be cheap and
//     is why it may not write. Proved by the same competing writer succeeding
//     when it lands inside read one.
//   - neither read happens after the gate has already refused. A refused
//     re-plan costs exactly one board read, not two, and still pulls no orders.
func TestTheBoardIsReadTwiceAndTheDecidingReadIsUnderTheDateClaim(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	// Asked of the lock itself rather than by sending a competing writer. A
	// writer that is correctly ALLOWED through read one would also PUSH, which
	// changes the board inside the very read under examination — the test would
	// then be describing a state it created.
	held := map[int]bool{}
	var second int
	d.g.onBoardRead = func(n int) {
		held[n] = d.store.holdsDate(d.date)
		if n == 2 {
			// End-to-end evidence for the read that matters: a real HTTP writer
			// for this date, refused from inside it.
			second = d.post("/api/v1/workflow/plans/plan-1/push", ``).Code
		}
	}
	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusCreated {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}

	if n := d.g.boardReads(); n != 2 {
		t.Fatalf("the board was read %d time(s) for one re-plan, want 2 (one to make the cheap refusal honest, one to decide under the claim)", n)
	}
	if held[1] {
		t.Errorf("the FIRST board read holds the dispatch date — the claim is deliberately started late, and taking it here serializes every push behind every re-plan across three ERP round-trips")
	}
	if !held[2] {
		t.Errorf("the DECIDING board read is taken OUTSIDE the dispatch-date claim, so the recall set is computed from a board another writer can still be moving")
	}
	if second != http.StatusConflict {
		t.Errorf("a competing writer for this date answered %d from inside the deciding read, want 409", second)
	}

	// A re-plan that is refused costs one board read and no order pull.
	d3 := newDispatchDay(t, planForTrucks("v1", "v2"))
	d3.push("plan-1")
	reads, pulls := d3.g.boardReads(), len(d3.g.orderDates)
	if rec := d3.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusLocked {
		t.Fatalf("want 423, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := d3.g.boardReads() - reads; n != 1 {
		t.Errorf("a refused re-plan read the board %d time(s), want 1 — it is refused before the deciding read is ever reached", n)
	}
	if n := len(d3.g.orderDates) - pulls; n != 0 {
		t.Errorf("the refused re-plan pulled orders %d time(s); the cheap gate must still sit before the day's planning data", n)
	}
}

// ---------------------------------------------------------------------------
// a route the recall key cannot name
// ---------------------------------------------------------------------------

// TestARouteWithNoVehicleIsReportedAndNeverGated covers the nullable column the
// ERP really has.
//
// delivery_routes.vehicle_id is nullable upstream, and the recall key is
// (vehicle_id, scheduled_date). A live route with no truck therefore cannot be
// named on the wire at all — there is no message this service could send that
// would withdraw it. Gating on it would be a refusal with no remedy, exactly
// like a departed truck; treating it as an orphan would put an empty vehicle id
// on a recall and cancel whatever the ERP matched against it.
func TestARouteWithNoVehicleIsReportedAndNeverGated(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.boardOnly("", gable.RouteStatusScheduled, "o-orphaned")

	if rec := d.ingest(`{"date":"2026-06-26"}`); rec.Code != http.StatusCreated {
		t.Fatalf("a route no recall could ever name must not gate the date: %d %s", rec.Code, rec.Body.String())
	}
	if got := d.g.recalledIDs(); len(got) != 0 {
		t.Fatalf("recalled %v — an empty vehicle id on the wire matches whatever the ERP decides it matches", got)
	}
	rep := d.report()
	div := rep.divergence(t, DivergenceUnaddressable)
	if div.VehicleID != "" || !strings.Contains(div.Note, "GableLBM") {
		t.Errorf("the report must say this one has to be fixed upstream: %+v", div)
	}
	if n := len(divergencesOfKind(rep.Divergences, DivergenceOrphan)); n != 0 {
		t.Errorf("a vehicle-less route reported as an ORPHAN would be proposed for a recall that cannot address it: %+v", rep.Divergences)
	}
}

// ---------------------------------------------------------------------------
// the report surface
// ---------------------------------------------------------------------------

// TestTheDispatchBoardReportIsWholeAndValidated checks the surface an orphan is
// surfaced ON, since a refusal only ever reaches somebody who happened to
// attempt a re-plan.
func TestTheDispatchBoardReportIsWholeAndValidated(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))
	d.push("plan-1")
	d.boardOnly("v9", gable.RouteStatusScheduled, "o-hand-written")

	rep := d.report()
	if rep.InSync {
		t.Errorf("a date holding an unnamed route is not in sync: %+v", rep)
	}
	if len(rep.Board) != 3 {
		t.Errorf("the report must carry the board as it stands, got %d rows: %+v", len(rep.Board), rep.Board)
	}
	if got := rep.Claims["v1"]; len(got) != 1 || got[0] != "plan-1" {
		t.Errorf("the report must say WHICH plan claims a truck, got %v", rep.Claims)
	}
	if div := rep.divergence(t, DivergenceOrphan); div.VehicleID != "v9" {
		t.Errorf("orphan: %+v", div)
	}

	// A date the ledger and the board agree on says so, positively.
	clean := newDispatchDay(t, planForTrucks("v1"))
	clean.push("plan-1")
	if rep := clean.report(); !rep.InSync || len(rep.Divergences) != 0 {
		t.Errorf("a date in agreement must report in_sync: %+v", rep)
	}

	// Validation matches the endpoint underneath: a dateless board is not a
	// smaller question, it is a meaningless one.
	for _, q := range []string{"", "?date=", "?date=26-06-2026", "?date=2026-13-01", "?date=tomorrow"} {
		if rec := d.get("/api/v1/workflow/dispatch-board" + q); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /dispatch-board%q answered %d, want 400", q, rec.Code)
		}
	}
}
