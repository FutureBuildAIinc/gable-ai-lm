// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// TestReassigningAPushedPlanLeavesOrphanRoutesAtTheDealer is a PIN, not a
// passing guard. It asserts the behaviour this module should have and skips,
// because choosing the right behaviour is a product decision rather than a
// missing guard clause.
//
// What happens today: Push writes one gable.DeliveryRoute per truck to
// GableLBM's dispatch board and marks the plan PUSHED. Assign then rebuilds
// p.Loads from scratch — new trucks, new stop splits — with no reference to
// what is already live upstream. Resequence at least NOTICES (it walks a
// PUSHED plan back to PACKED, service.go); Assign does not even do that.
//
// PushDeliveryRoute is idempotent upstream on (vehicle_id, scheduled_date), so
// a truck that survives the re-assignment is corrected on the next push. A
// truck the re-assignment DROPS is not: its route stays on the dealer's
// dispatch board, with its old stops and its old manifest, and nothing in this
// system will ever recall it. The yard loads a truck for a run that no longer
// exists.
//
// Why this is a pin and not a fix: there are at least three defensible
// answers and they are not equivalent.
//
//  1. Refuse. Assign on a PUSHED plan returns a Refusal telling the dispatcher
//     to unlock/recall first. Safe, and it makes "the customer called back at
//     10am" a support conversation.
//  2. Recall. Assign issues a delete/supersede upstream for every route it is
//     about to orphan. This is the right answer and GableLBM has no endpoint
//     for it — /api/integration/delivery-routes is create-or-replace only.
//  3. Warn and proceed, recording the orphaned (vehicle_id, date) pairs on the
//     plan so the next push can supersede them.
//
// (2) is blocked on an upstream capability, so the choice between (1) and (3)
// belongs to whoever owns the dispatcher's workflow. Until then this is stated
// rather than silently true.
func TestReassigningAPushedPlanLeavesOrphanRoutesAtTheDealer(t *testing.T) {
	t.Skip("KNOWN BUG: internal/workflow/service.go:Assign — a PUSHED plan is re-assigned with no refusal, no recall and no record, so a truck dropped by the re-assignment keeps a live route on GableLBM's dispatch board forever")

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

	// The assertion: re-assigning a plan whose routes are live upstream must
	// not be a silent no-questions rebuild.
	_, err = svc.Assign(context.Background(), "plan-1", false, "")
	if err == nil {
		t.Error("re-assigning a PUSHED plan must refuse (or record the routes it is orphaning), not rebuild silently")
	}
}
