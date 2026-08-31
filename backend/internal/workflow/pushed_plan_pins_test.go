// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// TestReassigningAPushedPlanLeavesOrphanRoutesAtTheDealer was a PIN: it stated
// the behaviour this module should have and skipped, because choosing that
// behaviour was a product decision rather than a missing guard clause. The
// decision has been taken and implemented, so it now runs.
//
// What used to happen: Push wrote one gable.DeliveryRoute per truck to
// GableLBM's dispatch board and marked the plan PUSHED. Assign then rebuilt
// p.Loads from scratch — new trucks, new stop splits — with no reference to
// what was already live upstream, discarding every LoadPlan, Compliance and
// Proof artifact on the way. Resequence at least NOTICED (it walked a PUSHED
// plan back to PACKED); Assign did not even do that.
//
// PushDeliveryRoute is idempotent upstream on (vehicle_id, scheduled_date), so
// a truck that SURVIVED the re-assignment was corrected on the next push. A
// truck the re-assignment DROPPED was not: its route stayed on the dealer's
// dispatch board, with its old stops and its old manifest, and nothing in this
// system would ever recall it. The yard loaded a truck for a run that no longer
// existed.
//
// THE DECISION. Of the three answers this pin used to list — refuse, recall, or
// warn-and-record — the shipped behaviour is refuse-by-default WITH recall on
// an explicit override, and the third is rejected outright:
//
//   - Assign (and Pack) on a plan whose routes are live REFUSE. The refusal is
//     ErrPushed, mapped to HTTP 423 with an approver named, which is the same
//     override idiom the T2-3 lock already uses — a dispatcher does not have to
//     learn a second way to say "yes, I mean it".
//   - On override, the routes of the trucks the new assignment DROPS are
//     recalled from GableLBM before the plan is rewritten, so the dispatch board
//     cannot outlive the plan that created it. Surviving trucks are NOT
//     recalled; their routes are stale, not orphaned, and the next push
//     replaces them.
//   - "Warn and proceed" was rejected. It leaves the wrong truck live on the
//     board and makes the yard's correctness depend on somebody reading a
//     warning, which is what this pin was filed about in the first place.
//
// Option (2) was blocked on an upstream capability when this was written.
// GableLBM now has it: POST /api/integration/delivery-routes/recall, keyed on
// (vehicle_id, scheduled_date), idempotent, with 409 reserved for a truck that
// has already left the yard. See gable.RecallDeliveryRoute.
func TestReassigningAPushedPlanLeavesOrphanRoutesAtTheDealer(t *testing.T) {
	store := newFakePlanStore(pushReadyPlan())
	g := &fakeGable{
		vehicles: []gable.Vehicle{{ID: "v1", Name: "Flatbed 1", CapacityWeightLbs: intPtr(20000)}},
		drivers:  []gable.Driver{{ID: "d1", Name: "Sam", Status: "ACTIVE"}},
	}
	svc := newTestService(store, g, Config{})

	pushed, err := svc.Push(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if pushed.Status != StatusPushed || len(g.pushed) != 1 {
		t.Fatalf("setup: status=%q routes=%d", pushed.Status, len(g.pushed))
	}

	// The assertion this pin was filed for: re-assigning a plan whose routes
	// are live upstream must not be a silent no-questions rebuild.
	before := store.stored("plan-1")
	_, err = svc.Assign(context.Background(), "plan-1", false, "")
	if err == nil {
		t.Fatal("re-assigning a PUSHED plan must refuse, not rebuild silently")
	}
	if !errors.Is(err, ErrPushed) {
		t.Errorf("the refusal must be ErrPushed so the handler answers 423 and the UI prompts for approval, got %v", err)
	}

	// A refusal that half-mutated the plan would pass the assertion above and
	// still be the bug. The stored plan must be untouched.
	after := store.stored("plan-1")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("a refused re-assignment must write NOTHING\n before: %+v\n  after: %+v", before, after)
	}
	if len(g.recalled) != 0 {
		t.Errorf("a refused re-assignment must not touch the dispatch board, recalled %v", g.recalledIDs())
	}
}
