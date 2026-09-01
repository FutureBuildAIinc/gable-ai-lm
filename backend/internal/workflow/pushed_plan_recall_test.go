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
	"strings"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// This file covers the plan state machine: what each transition is legal from,
// what a refusal is allowed to write (nothing), what an approved change does to
// the routes already on the dealer's dispatch board, and what a push that dies
// half-way leaves behind.

// multiTruckPushReadyPlan builds a REVIEWED, packed, signed plan carrying n
// trucks, each with its own order and stop — the shape a push walks.
func multiTruckPushReadyPlan(n int) *Plan {
	base := pushReadyPlan()
	p := &Plan{
		ID: "plan-1", PlanDate: base.PlanDate, Status: StatusReviewed, Version: 1,
		DepotLat: base.DepotLat, DepotLng: base.DepotLng,
		UnassignedOrders: []Stop{},
	}
	for i := 0; i < n; i++ {
		orderID := fmt.Sprintf("o%d", i+1)
		vehicleID := fmt.Sprintf("v%d", i+1)
		order := base.Orders[0]
		order.OrderID = orderID
		order.CustomerName = fmt.Sprintf("Customer %d", i+1)
		p.Orders = append(p.Orders, order)

		load := base.Loads[0]
		load.VehicleID = vehicleID
		load.VehicleName = fmt.Sprintf("Flatbed %d", i+1)
		load.DriverID = fmt.Sprintf("d%d", i+1)
		load.Stops = []Stop{{OrderID: orderID, Sequence: 1, Lat: 49.1, Lng: -119.1, WeightLbs: 4000}}
		p.Loads = append(p.Loads, load)
	}
	return p
}

// fleetOf builds a GableLBM fleet double covering the named vehicles.
//
// The rated capacity is deliberately tight. sweepAssign only fills a truck to
// cargoUtilizationCap (80%) of its rating, so 5,000 lb rated is 4,000 lb
// usable — exactly one of these fixture orders. That makes "which trucks does
// this assignment use?" a property of the FLEET rather than of the packer's
// arithmetic, which is what the recall assertions need to control.
func fleetOf(ids ...string) *fakeGable {
	g := &fakeGable{}
	for i, id := range ids {
		g.vehicles = append(g.vehicles, gable.Vehicle{
			ID: id, Name: fmt.Sprintf("Flatbed %s", strings.TrimPrefix(id, "v")),
			VehicleType: "FLATBED", CapacityWeightLbs: intPtr(5000),
		})
		g.drivers = append(g.drivers, gable.Driver{
			ID: fmt.Sprintf("d%d", i+1), Name: fmt.Sprintf("Driver %d", i+1), Status: "ACTIVE",
		})
	}
	return g
}

// pushPlan drives a plan onto the fake dispatch board and fails the test if it
// does not get there, so the assertions below start from a known live board.
func pushPlan(t *testing.T, svc *Service, store *fakePlanStore) *Plan {
	t.Helper()
	p, err := svc.Push(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("setup push: %v", err)
	}
	if p.Status != StatusPushed {
		t.Fatalf("setup push left status %q, want %q", p.Status, StatusPushed)
	}
	return store.stored("plan-1")
}

// TestEveryRefusedTransitionWritesNothing is the guarantee the whole state
// machine rests on. A gate that refuses but has already half-mutated the plan
// is not a gate — it is the original defect with an error message attached, and
// it would pass any test that only asserted "an error came back".
//
// So every case here compares the ENTIRE stored plan before and after, and
// asserts the dispatch board was not touched either.
func TestEveryRefusedTransitionWritesNothing(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		invoke  func(svc *Service) error
		wantMsg string
	}{{
		name:   "assign on a pushed plan without approval",
		status: StatusPushed,
		invoke: func(svc *Service) error {
			_, err := svc.Assign(context.Background(), "plan-1", false, "")
			return err
		},
		wantMsg: "manual approval",
	}, {
		name:   "pack on a pushed plan without approval",
		status: StatusPushed,
		invoke: func(svc *Service) error {
			_, err := svc.Pack(context.Background(), "plan-1", false, "")
			return err
		},
		wantMsg: "manual approval",
	}, {
		name:   "resequence on a pushed plan without approval",
		status: StatusPushed,
		invoke: func(svc *Service) error {
			_, err := svc.Resequence(context.Background(), "plan-1", "v1", []string{"o1"}, false, "")
			return err
		},
		wantMsg: "manual approval",
	}, {
		name:   "priority change on a pushed plan without approval",
		status: StatusPushed,
		invoke: func(svc *Service) error {
			_, err := svc.SetPriority(context.Background(), "plan-1", "o1", true, false, "")
			return err
		},
		wantMsg: "manual approval",
	}, {
		name:   "dimension override on a pushed plan without approval",
		status: StatusPushed,
		invoke: func(svc *Service) error {
			_, err := svc.SetLineDimensions(context.Background(), "plan-1", "o1",
				DimensionOverrideRequest{SKU: "2x4-8", LengthIn: 96, WidthIn: 4, HeightIn: 2})
			return err
		},
		wantMsg: "manual approval",
	}, {
		name:   "review on a pushed plan is refused outright, not offered an override",
		status: StatusPushed,
		invoke: func(svc *Service) error {
			_, err := svc.Review(context.Background(), "plan-1")
			return err
		},
		wantMsg: "already on the dispatch board",
	}, {
		name:   "re-push of a plan already on the board",
		status: StatusPushed,
		invoke: func(svc *Service) error {
			_, err := svc.Push(context.Background(), "plan-1")
			return err
		},
		wantMsg: "already on the dispatch board",
	}, {
		name:   "push before review",
		status: StatusPacked,
		invoke: func(svc *Service) error {
			_, err := svc.Push(context.Background(), "plan-1")
			return err
		},
		wantMsg: "run review first",
	}, {
		name:   "pack before assign",
		status: StatusAnalyzed,
		invoke: func(svc *Service) error {
			_, err := svc.Pack(context.Background(), "plan-1", false, "")
			return err
		},
		wantMsg: "run assign first",
	}, {
		name:   "review before pack",
		status: StatusAssigned,
		invoke: func(svc *Service) error {
			_, err := svc.Review(context.Background(), "plan-1")
			return err
		},
		wantMsg: "run pack first",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := multiTruckPushReadyPlan(1)
			p.Status = tc.status
			if tc.status == StatusPushed {
				// A plan is only genuinely on the board if the ledger says so.
				p.LiveRoutes = []LiveRoute{{VehicleID: "v1", VehicleName: "Flatbed 1"}}
			}
			store := newFakePlanStore(p)
			g := fleetOf("v1")
			svc := newTestService(store, g, Config{})

			before := store.stored("plan-1")
			err := tc.invoke(svc)
			if err == nil {
				t.Fatal("this transition must be refused")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal %q should contain %q — it is the sentence the dispatcher acts on", err, tc.wantMsg)
			}

			if after := store.stored("plan-1"); !reflect.DeepEqual(before, after) {
				t.Errorf("a refused transition must write NOTHING\n before: %+v\n  after: %+v", before, after)
			}
			if len(g.pushed) != 0 || len(g.recalled) != 0 {
				t.Errorf("a refused transition must not touch the dispatch board: pushed=%v recalled=%v",
					g.pushedIDs(), g.recalledIDs())
			}
		})
	}
}

// TestOverrideRecallsExactlyTheDroppedTrucks is the behaviour the whole change
// exists to produce, and the assertion that matters most is the negative one: a
// truck that SURVIVES the re-assignment must never be recalled. Recalling a
// surviving truck would cancel a route the dealer still needs — a worse failure
// than the orphan this work set out to fix, because it takes a good run off the
// board silently.
func TestOverrideRecallsExactlyTheDroppedTrucks(t *testing.T) {
	store := newFakePlanStore(multiTruckPushReadyPlan(3))
	g := fleetOf("v1", "v2", "v3")
	svc := newTestService(store, g, Config{})
	pushPlan(t, svc, store)

	if got := g.pushedIDs(); len(got) != 3 {
		t.Fatalf("setup: dispatch board holds %v, want three routes", got)
	}

	// The fleet shrinks: v2 goes out of service, so a re-assignment can only
	// place the work on v1 and v3.
	g.vehicles = []gable.Vehicle{g.vehicles[0], g.vehicles[2]}

	got, err := svc.Assign(context.Background(), "plan-1", true, "dispatcher@dealer.com")
	if err != nil {
		t.Fatalf("an approved re-assignment must proceed: %v", err)
	}

	if ids := g.recalledIDs(); !reflect.DeepEqual(ids, []string{"v2"}) {
		t.Errorf("recalled %v, want exactly [v2] — the only truck the re-assignment dropped", ids)
	}
	for _, r := range g.recalled {
		if r.ScheduledDate != got.PlanDate {
			t.Errorf("recall keyed on date %q, want the plan's date %q", r.ScheduledDate, got.PlanDate)
		}
		if r.RecalledBy != "dispatcher@dealer.com" {
			t.Errorf("recall must carry the approver, got %q", r.RecalledBy)
		}
		if r.Reason == "" {
			t.Error("recall must say why the route was withdrawn")
		}
	}

	// The ledger tombstones the recall rather than forgetting it: support has
	// to be able to answer "what did we put on their board, and when did we
	// take it off".
	stored := store.stored("plan-1")
	var tombstoned, stillLive []string
	for _, r := range stored.LiveRoutes {
		if r.Live() {
			stillLive = append(stillLive, r.VehicleID)
			continue
		}
		tombstoned = append(tombstoned, r.VehicleID)
		if r.RecalledBy != "dispatcher@dealer.com" || r.RecallNote == "" {
			t.Errorf("tombstone %+v must record who approved the recall and why", r)
		}
	}
	if !reflect.DeepEqual(tombstoned, []string{"v2"}) {
		t.Errorf("tombstoned %v, want [v2]", tombstoned)
	}
	if len(stillLive) != 2 {
		t.Errorf("v1 and v3 are stale, not orphaned — they must stay on the ledger, got %v", stillLive)
	}

	// And the approval itself is on the plan, not only in a log line.
	if len(stored.PushedOverrides) != 1 || stored.PushedOverrides[0].ApprovedBy != "dispatcher@dealer.com" {
		t.Errorf("the approval must be recorded on the plan, got %+v", stored.PushedOverrides)
	}
	if stored.Status != StatusAssigned {
		t.Errorf("status = %q, want %q", stored.Status, StatusAssigned)
	}
}

// TestARecallFailureAbortsTheWholeReassignment pins the rule that a
// half-recalled board is worse than a stale one. If the recall cannot be
// completed, the plan must be left exactly as it was — still naming every route
// as live, so the next attempt recalls them all again. Recall is idempotent
// upstream precisely so that over-reporting is the safe direction: it costs a
// redundant call. Under-reporting costs a truck loading for a run that no
// longer exists.
func TestARecallFailureAbortsTheWholeReassignment(t *testing.T) {
	store := newFakePlanStore(multiTruckPushReadyPlan(2))
	g := fleetOf("v1", "v2")
	svc := newTestService(store, g, Config{})
	pushPlan(t, svc, store)

	before := store.stored("plan-1")
	g.vehicles = g.vehicles[:1] // v2 is dropped, so its route needs recalling
	g.recallErr = errors.New("gable POST /api/integration/delivery-routes/recall: status 503: unavailable")

	if _, err := svc.Assign(context.Background(), "plan-1", true, "dispatcher@dealer.com"); err == nil {
		t.Fatal("a re-assignment whose recall failed must not succeed")
	}

	after := store.stored("plan-1")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("a failed recall must leave the plan exactly as it was\n before: %+v\n  after: %+v", before, after)
	}
	if len(after.LiveRoutes) != 2 {
		t.Fatalf("the ledger must still name both routes as live, got %+v", after.LiveRoutes)
	}
	for _, r := range after.LiveRoutes {
		if !r.Live() {
			t.Errorf("route %s must not be tombstoned by a recall that never happened", r.VehicleID)
		}
	}
	if after.Status != StatusPushed {
		t.Errorf("status = %q, want it left at %q", after.Status, StatusPushed)
	}

	// And the retry converges once GableLBM is reachable again.
	g.recallErr = nil
	if _, err := svc.Assign(context.Background(), "plan-1", true, "dispatcher@dealer.com"); err != nil {
		t.Fatalf("the retry must succeed once the recall endpoint is reachable: %v", err)
	}
	if ids := g.recalledIDs(); !reflect.DeepEqual(ids, []string{"v2"}) {
		t.Errorf("the retry recalled %v, want [v2]", ids)
	}
}

// TestARecalledTruckAlreadyOnTheRoadIsTerminal pins that the one failure a
// retry cannot fix is reported as a sentence a human can act on. The truck has
// left the yard; looping on the recall would never converge.
func TestARecalledTruckAlreadyOnTheRoadIsTerminal(t *testing.T) {
	store := newFakePlanStore(multiTruckPushReadyPlan(2))
	g := fleetOf("v1", "v2")
	svc := newTestService(store, g, Config{})
	pushPlan(t, svc, store)

	before := store.stored("plan-1")
	g.vehicles = g.vehicles[:1]
	g.recallDispatched = true

	_, err := svc.Assign(context.Background(), "plan-1", true, "dispatcher@dealer.com")
	if err == nil {
		t.Fatal("a truck already on the road must stop the re-assignment")
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("this must be a Refusal so the dispatcher reads it verbatim, got %T: %v", err, err)
	}
	if !strings.Contains(refusal.Msg, "left the yard") {
		t.Errorf("refusal %q should say the truck has left the yard", refusal.Msg)
	}
	if after := store.stored("plan-1"); !reflect.DeepEqual(before, after) {
		t.Error("a terminal recall failure must still leave the plan untouched")
	}
}

// TestAPartialPushIsResumable is the third defect in the family. The write loop
// used to return on the FIRST error with p.Status set only afterwards, so a
// push that died on truck 2 of 3 left trucks 1 live upstream, the plan reading
// REVIEWED, and no per-load record of what had actually been written. Nothing
// was resumable and nothing was recallable — the plan did not know those routes
// existed.
func TestAPartialPushIsResumable(t *testing.T) {
	store := newFakePlanStore(multiTruckPushReadyPlan(3))
	g := fleetOf("v1", "v2", "v3")
	g.pushErrAfter = 2 // trucks 1 and 2 land; truck 3 fails
	svc := newTestService(store, g, Config{})

	_, err := svc.Push(context.Background(), "plan-1")
	if err == nil {
		t.Fatal("a push that could not write every truck must not report success")
	}
	if !strings.Contains(err.Error(), "run push again") {
		t.Errorf("refusal %q must tell the dispatcher the run can be resumed", err)
	}
	if !strings.Contains(err.Error(), "Flatbed 3") {
		t.Errorf("refusal %q must name the truck it stopped at", err)
	}

	stored := store.stored("plan-1")
	// The status must describe the dispatch board, and the board is only
	// partly written.
	if stored.Status == StatusPushed {
		t.Error("status must never read PUSHED when only some trucks were written")
	}
	if stored.Status != StatusReviewed {
		t.Errorf("status = %q, want it left at %q so the retry is an ordinary push", stored.Status, StatusReviewed)
	}
	// What DID land is recorded, per truck and on the ledger.
	if got := len(liveRoutes(stored)); got != 2 {
		t.Fatalf("the ledger records %d live route(s), want 2 — this is the record that makes a recall possible", got)
	}
	for i, want := range []bool{true, true, false} {
		if acked := stored.Loads[i].PushedAt != nil; acked != want {
			t.Errorf("load %s acked = %v, want %v", stored.Loads[i].VehicleName, acked, want)
		}
	}

	// Resume: GableLBM comes back, and the re-push writes ONLY the truck that
	// never landed. Re-POSTing the first two would be wasteful and would churn
	// routes the yard may already be working from.
	// Counted in CALLS, not in board size: the board replaces per (truck, date)
	// exactly as GableLBM does, so a re-sent truck leaves its length unchanged.
	g.pushErrAfter = 0
	before := g.pushCalls
	got, err := svc.Push(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("the resumed push must complete the run: %v", err)
	}
	if wrote := g.pushCalls - before; wrote != 1 {
		t.Errorf("the resume wrote %d route(s), want 1 — the two already live must be skipped", wrote)
	}
	if got.Status != StatusPushed {
		t.Errorf("status = %q, want %q once every truck is on the board", got.Status, StatusPushed)
	}
	if n := len(liveRoutes(store.stored("plan-1"))); n != 3 {
		t.Errorf("ledger holds %d live routes, want 3", n)
	}
}

// TestAResumeRePushesATruckWhoseRouteChanged is the safety catch on the resume
// optimization. Skipping is authorized by the DIGEST, not the timestamp: a
// truck that was pushed and has since had its route changed must be written
// again, or it keeps a stale manifest upstream while the plan believes it is
// current. A timestamp-only check would have skipped it.
func TestAResumeRePushesATruckWhoseRouteChanged(t *testing.T) {
	store := newFakePlanStore(multiTruckPushReadyPlan(2))
	g := fleetOf("v1", "v2")
	g.pushErrAfter = 1 // only truck 1 lands
	svc := newTestService(store, g, Config{})

	if _, err := svc.Push(context.Background(), "plan-1"); err == nil {
		t.Fatal("setup: the push was supposed to fail part-way")
	}

	// Truck 1 is acked. Now its route changes underneath: a stop moves onto it.
	p := store.stored("plan-1")
	if p.Loads[0].PushedAt == nil {
		t.Fatal("setup: truck 1 should have been acked")
	}
	p.Loads[0].Stops = append(p.Loads[0].Stops,
		Stop{OrderID: "o2", Sequence: 2, Lat: 49.3, Lng: -119.3, WeightLbs: 1000})
	if err := store.Update(context.Background(), p); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Counted in CALLS, not in board size: re-sending truck 1 REPLACES its row
	// upstream (that is what ReplaceDeliveryRoute does), so the board grows by
	// one while two routes are written.
	g.pushErrAfter = 0
	before := g.pushCalls
	if _, err := svc.Push(context.Background(), "plan-1"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if wrote := g.pushCalls - before; wrote != 2 {
		t.Errorf("wrote %d route(s), want 2 — the acked truck changed and must be re-sent, not skipped", wrote)
	}
}

// TestApprovedRepackDoesNotRecallAnything pins the other half of the product
// decision. Re-packing changes what is ON each truck, never which trucks exist,
// so nothing is orphaned and nothing may be withdrawn from the board. The
// routes go stale and the next push replaces them.
func TestApprovedRepackDoesNotRecallAnything(t *testing.T) {
	store := newFakePlanStore(multiTruckPushReadyPlan(2))
	g := fleetOf("v1", "v2")
	svc := newTestService(store, g, Config{})
	pushPlan(t, svc, store)

	got, err := svc.Pack(context.Background(), "plan-1", true, "yard.lead@dealer.com")
	if err != nil {
		t.Fatalf("an approved re-pack must proceed: %v", err)
	}
	if len(g.recalled) != 0 {
		t.Errorf("a re-pack drops no truck and must recall nothing, recalled %v", g.recalledIDs())
	}
	if got.Status != StatusPacked {
		t.Errorf("status = %q, want %q", got.Status, StatusPacked)
	}
	stored := store.stored("plan-1")
	if n := len(liveRoutes(stored)); n != 2 {
		t.Errorf("the routes really are still on the dealer's board — the ledger must keep them, got %d", n)
	}
	if len(stored.PushedOverrides) != 1 || stored.PushedOverrides[0].Action != actionPack {
		t.Errorf("the approval must be recorded on the plan, got %+v", stored.PushedOverrides)
	}
}

// TestAPartiallyPushedPlanStillGatesAReassignment is the case a status-only
// check would miss. After a partial push the status is REVIEWED, not PUSHED —
// but trucks really are live upstream, and re-assigning orphans them exactly as
// much. The gate keys on the ledger as well as the status.
func TestAPartiallyPushedPlanStillGatesAReassignment(t *testing.T) {
	store := newFakePlanStore(multiTruckPushReadyPlan(3))
	g := fleetOf("v1", "v2", "v3")
	g.pushErrAfter = 1
	svc := newTestService(store, g, Config{})

	if _, err := svc.Push(context.Background(), "plan-1"); err == nil {
		t.Fatal("setup: the push was supposed to fail part-way")
	}
	if store.stored("plan-1").Status != StatusReviewed {
		t.Fatal("setup: a partial push should leave the plan at REVIEWED")
	}

	before := store.stored("plan-1")
	_, err := svc.Assign(context.Background(), "plan-1", false, "")
	if !errors.Is(err, ErrPushed) {
		t.Fatalf("a partially pushed plan must still refuse an unapproved re-assignment, got %v", err)
	}
	if after := store.stored("plan-1"); !reflect.DeepEqual(before, after) {
		t.Error("the refusal must write nothing")
	}
}

// TestApprovingALateAddOnAPushedRunRecallsTheDroppedTrucks pins a decision
// worth stating rather than discovering. ResolveLateAdd re-assigns with
// override hard-coded true, so approving a late order on a run that is already
// on the board now also authorizes the recalls that re-assignment implies.
//
// That is deliberate: the approver is named, the recall is tombstoned against
// them, and the alternative — a second, separate override for the same human
// decision — would double the signature of four methods to ask a question the
// approval has already answered.
func TestApprovingALateAddOnAPushedRunRecallsTheDroppedTrucks(t *testing.T) {
	p := multiTruckPushReadyPlan(2)
	p.LateAdds = []LateAdd{{OrderID: "o2", Status: LateAddPending}}
	store := newFakePlanStore(p)
	g := fleetOf("v1", "v2")
	svc := newTestService(store, g, Config{})
	pushPlan(t, svc, store)

	g.vehicles = g.vehicles[:1] // only v1 survives the reshuffle

	if _, err := svc.ResolveLateAdd(context.Background(), "plan-1", "o2",
		LateAddApproveRequest{ApprovedBy: "night.dispatch@dealer.com"}); err != nil {
		t.Fatalf("resolve late add: %v", err)
	}

	if ids := g.recalledIDs(); !reflect.DeepEqual(ids, []string{"v2"}) {
		t.Errorf("recalled %v, want [v2]", ids)
	}
	for _, r := range g.recalled {
		if r.RecalledBy != "night.dispatch@dealer.com" {
			t.Errorf("the recall must be attributed to the approver, got %q", r.RecalledBy)
		}
	}
}

// TestPushedRefusalsAnswer423 pins the wire contract. The UI has exactly one
// override prompt, and it opens on 423; a pushed-plan refusal that answered 422
// would be a dead end on screen with no way for the dispatcher to proceed.
func TestPushedRefusalsAnswer423(t *testing.T) {
	cases := []struct {
		name, method, path string
		wantStatus         int
	}{
		{"assign", http.MethodPost, "/api/v1/workflow/plans/plan-1/assign", http.StatusLocked},
		{"pack", http.MethodPost, "/api/v1/workflow/plans/plan-1/pack", http.StatusLocked},
		// Push has nothing to approve, so it is a plain refusal, not a prompt.
		{"push", http.MethodPost, "/api/v1/workflow/plans/plan-1/push", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := multiTruckPushReadyPlan(1)
			p.Status = StatusPushed
			p.LiveRoutes = []LiveRoute{{VehicleID: "v1", VehicleName: "Flatbed 1"}}
			store := newFakePlanStore(p)
			svc := newTestService(store, fleetOf("v1"), Config{})

			mux := http.NewServeMux()
			NewHandler(svc).RegisterRoutes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// The body must carry the module's own sentence, never a generic
			// "workflow step failed" the operator cannot act on.
			if !strings.Contains(rec.Body.String(), "dispatch board") {
				t.Errorf("body %s should explain that the routes are live", rec.Body.String())
			}
		})
	}
}

// TestPayloadRoundTripsTheLiveRouteLedger is the only test that can catch the
// repository payload trap. Plan-level fields are enumerated by hand in three
// places (the payload struct, marshalPayload, unmarshalPayload) and a field
// missing from any of them is silently dropped on every write — while every
// in-memory test still passes, because fakePlanStore round-trips the whole Plan
// through encoding/json and never touches that code.
//
// The ledger is exactly the field that must not be lost: it is the only record
// of what this plan put on the dealer's dispatch board.
func TestPayloadRoundTripsTheLiveRouteLedger(t *testing.T) {
	recalledAt := timePtr()
	in := &Plan{
		ID: "plan-1", PlanDate: "2026-06-26", Status: StatusAssigned,
		Orders: []OrderAnalysis{}, Loads: []TruckLoad{{
			VehicleID: "v1", VehicleName: "Flatbed 1", PushedAt: recalledAt, PushedDigest: "abc123",
		}},
		UnassignedOrders: []Stop{},
		LiveRoutes: []LiveRoute{
			{VehicleID: "v1", VehicleName: "Flatbed 1", PushedAt: *recalledAt},
			{VehicleID: "v2", VehicleName: "Flatbed 2", PushedAt: *recalledAt,
				RecalledAt: recalledAt, RecalledBy: "dispatcher@dealer.com", RecallNote: "dropped by re-assigning trucks"},
		},
		PushedOverrides: []PushedOverride{{
			Action: actionAssign, ApprovedBy: "dispatcher@dealer.com", ApprovedAt: *recalledAt,
		}},
		BoardRepairs: []BoardRepair{{
			Kind: DivergenceOrphan, Action: RepairRecalled, VehicleID: "v9",
			RouteID: "route-9", BoardStatus: "SCHEDULED", StopCount: 2,
			OrderIDs: []string{"o-1", "o-2"}, At: *recalledAt, By: "dispatcher@dealer.com",
			Note: "withdrawn by an approved re-plan",
		}},
	}

	r := &Repository{}
	raw, err := r.marshalPayload(in)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	var out Plan
	if err := r.unmarshalPayload(raw, &out); err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}

	if len(out.LiveRoutes) != 2 {
		t.Fatalf("LiveRoutes did not survive the payload round-trip: got %+v.\n"+
			"Add the field to the payload struct AND marshalPayload AND unmarshalPayload — all three.", out.LiveRoutes)
	}
	if out.LiveRoutes[0].VehicleID != "v1" || !out.LiveRoutes[0].Live() {
		t.Errorf("live route lost: %+v", out.LiveRoutes[0])
	}
	if out.LiveRoutes[1].Live() || out.LiveRoutes[1].RecalledBy != "dispatcher@dealer.com" {
		t.Errorf("recall tombstone lost: %+v", out.LiveRoutes[1])
	}
	if len(out.PushedOverrides) != 1 || out.PushedOverrides[0].ApprovedBy != "dispatcher@dealer.com" {
		t.Errorf("PushedOverrides did not survive the payload round-trip: %+v", out.PushedOverrides)
	}
	if out.Loads[0].PushedAt == nil || out.Loads[0].PushedDigest != "abc123" {
		t.Errorf("the per-load ack must ride inside Loads: %+v", out.Loads[0])
	}
	// BoardRepairs is the ONLY record that a route was taken off the dealer's
	// board that no plan named, and who approved it. Losing it to the payload
	// trap would leave an incident review unable to answer "who cancelled this
	// customer's delivery?" — and every in-memory test would still pass.
	if len(out.BoardRepairs) != 1 {
		t.Fatalf("BoardRepairs did not survive the payload round-trip: got %+v.\n"+
			"Add the field to the payload struct AND marshalPayload AND unmarshalPayload — all three.", out.BoardRepairs)
	}
	if r := out.BoardRepairs[0]; r.Kind != DivergenceOrphan || r.Action != RepairRecalled ||
		r.VehicleID != "v9" || r.By != "dispatcher@dealer.com" || len(r.OrderIDs) != 2 {
		t.Errorf("board repair lost detail in the round-trip: %+v", r)
	}
}

// TestEveryStatusWriteComesFromTheTable guards against the machine drifting
// back into scattered assignments. Every status the four fixed-status steps
// write must be the one the table declares.
func TestEveryStatusWriteComesFromTheTable(t *testing.T) {
	for action, want := range map[string]string{
		actionAssign: StatusAssigned,
		actionPack:   StatusPacked,
		actionReview: StatusReviewed,
		actionPush:   StatusPushed,
	} {
		if got := planTransitions[action].to; got != want {
			t.Errorf("planTransitions[%q].to = %q, want %q", action, got, want)
		}
	}
	// A status nobody declared must fail closed, not run ungated.
	p := &Plan{Status: "SOMETHING_NEW"}
	if err := gateTransition(p, actionPush, false, ""); err == nil {
		t.Error("an undeclared status must be refused, not permitted by default")
	}
	if err := gateTransition(&Plan{Status: StatusReviewed}, "teleport", false, ""); err == nil {
		t.Error("an action with no declared row must be refused, not run ungated")
	}
}

// timePtr is a fixed instant for round-trip fixtures.
func timePtr() *time.Time {
	t := time.Date(2026, 6, 26, 8, 0, 0, 0, time.UTC)
	return &t
}
