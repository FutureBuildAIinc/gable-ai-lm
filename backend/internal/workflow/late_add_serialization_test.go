// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"net/http"
	"testing"
)

// Resolving a late add is a writer to the dispatch date, and it used to take the
// claim HALFWAY THROUGH ITSELF.
//
// The sequence was: read the plan, mark the queue entry APPROVED, COMMIT, then
// call Assign — which takes the claim, and refuses if somebody else holds the
// date. So a dispatcher who lost that race was handed the DATE_BUSY sentence
//
//	"...Nothing was sent to GableLBM and nothing on the dispatch board changed
//	 — wait a moment and try again."
//
// under a Try again button, with the late add already resolved. Two things were
// false at once: the sentence, and the button. Pressing it answered 422 ("late
// add for order o-v2 is already APPROVED") for ever, and the order sat approved
// on a run nothing had reshuffled.
//
// It produced no acceptance violation in twelve thousand concurrency trials,
// which is exactly why it needs deterministic tests rather than more trials:
// nothing about it is a race the oracle can see. It is a promise that was not
// kept.

// TestARefusedLateAddResolutionKeepsItsPromise is acceptance IV for this path,
// stated as the two things the refusal claims.
func TestARefusedLateAddResolutionKeepsItsPromise(t *testing.T) {
	d := newDispatchDay(t, planWithAQueuedLateAdd())
	d.push("plan-1")

	before := d.store.stored("plan-1")
	d.store.mu.Lock()
	savesBefore := d.store.updates
	d.store.mu.Unlock()

	var code int
	err := d.store.WithDateLock(context.Background(), d.date, func(context.Context) error {
		code = d.post("/api/v1/workflow/plans/plan-1/late-adds/o-v2/resolve", `{"approved_by":"night.dispatch@dealer.com"}`).Code
		return nil
	})
	if err != nil {
		t.Fatalf("holding the date should succeed: %v", err)
	}
	if code != http.StatusConflict {
		t.Fatalf("resolving a late add on a held date answered %d, want 409", code)
	}

	// "nothing on the dispatch board changed"
	after := d.store.stored("plan-1")
	if got := after.LateAdds[0].Status; got != LateAddPending {
		t.Errorf("the refused resolution left the late add %s — the sentence promises nothing changed, and the Try again button repeats an action that now answers 422 for ever", got)
	}
	if after.Version != before.Version {
		t.Errorf("the refused resolution advanced the plan from version %d to %d", before.Version, after.Version)
	}
	d.store.mu.Lock()
	saves := d.store.updates
	d.store.mu.Unlock()
	if saves != savesBefore {
		t.Errorf("the refused resolution wrote the plan store %d time(s)", saves-savesBefore)
	}
	if n := len(d.g.recalledIDs()); n != 0 {
		t.Errorf("the refused resolution recalled %d route(s) from the dealer's board", n)
	}

	// "try again" — the whole point of failing fast is that the repeat is plain.
	rec := d.post("/api/v1/workflow/plans/plan-1/late-adds/o-v2/resolve", `{"approved_by":"night.dispatch@dealer.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the retry of a refused late-add resolution answered %d (%s) — the button the refusal offers must work", rec.Code, rec.Body.String())
	}
	if got := d.store.stored("plan-1").LateAdds[0].Status; got != LateAddApproved {
		t.Errorf("after a successful retry the late add is %s, want %s", got, LateAddApproved)
	}
	d.assertAcceptance()
}

// TestTheLateAddResolutionClaimsTheDateBeforeItWritesAnything is the seam.
//
// A competing push is landed at the exact instant the resolution is about to
// commit its FIRST piece of state. If the claim is taken before that write, the
// push cannot get in and answers 409. If the claim is taken later — as it was —
// the push sails through, and the resolution goes on to commit state it has
// promised it would not.
//
// This is the deterministic statement of the defect. It does not depend on a
// scheduler, and it fails at the previous commit.
func TestTheLateAddResolutionClaimsTheDateBeforeItWritesAnything(t *testing.T) {
	d := newDispatchDay(t, planWithAQueuedLateAdd(), planForTrucks("v1", "v2"))
	d.push("plan-1")

	var competing int
	d.store.beforeUpdate = func() {
		competing = d.post("/api/v1/workflow/plans/plan-2/push", ``).Code
	}

	rec := d.post("/api/v1/workflow/plans/plan-1/late-adds/o-v2/resolve", `{"approved_by":"night.dispatch@dealer.com"}`)

	if competing == 0 {
		t.Fatal("the competing push never ran — this test asserted nothing")
	}
	if competing != http.StatusConflict {
		t.Errorf("a push landing at the resolution's first write answered %d, want 409 — the late add is committed outside the claim, so the refusal it hands the loser (\"nothing changed\") is false", competing)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("the resolution itself must still proceed: %d %s", rec.Code, rec.Body.String())
	}
	d.assertAcceptance()
}

// TestAnApprovedLateAddRunsItsReassignmentUnderTheClaimItAlreadyHolds.
//
// Resolving is one operation to the dispatcher and two to this package: the
// queue entry, then the re-assignment that acts on it. Both now run under ONE
// claim. The failure this guards against is the obvious implementation of the
// fix: claim the date in ResolveLateAdd, then call Assign, which asks for the
// SAME date again and is refused BY ITSELF — an operation that can never
// succeed, and which no existing test would have caught because every one of
// them resolves a late add on an uncontended date.
func TestAnApprovedLateAddRunsItsReassignmentUnderTheClaimItAlreadyHolds(t *testing.T) {
	d := newDispatchDay(t, planWithAQueuedLateAdd())
	d.push("plan-1")

	grantsBefore, _ := d.store.lockCounts()
	rec := d.post("/api/v1/workflow/plans/plan-1/late-adds/o-v2/resolve", `{"approved_by":"night.dispatch@dealer.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approving a late add answered %d (%s)", rec.Code, rec.Body.String())
	}
	grants, refusals := d.store.lockCounts()
	if got := grants - grantsBefore; got != 1 {
		t.Errorf("one dispatcher action took %d claims on the date; it must take exactly 1 and re-enter it", got)
	}
	if refusals != 0 {
		t.Errorf("the resolution refused itself %d time(s)", refusals)
	}
	if got := d.store.stored("plan-1").LateAdds[0].Status; got != LateAddApproved {
		t.Errorf("the late add is %s, want %s", got, LateAddApproved)
	}
}

// TestARejectedLateAddIsAlsoClaimed. Rejection writes less than approval — it
// drops the order and never re-assigns — but it still commits state under the
// same promise, so it must be refused the same way rather than half-applied.
func TestARejectedLateAddIsAlsoClaimed(t *testing.T) {
	d := newDispatchDay(t, planWithAQueuedLateAdd())
	d.push("plan-1")

	var code int
	_ = d.store.WithDateLock(context.Background(), d.date, func(context.Context) error {
		code = d.post("/api/v1/workflow/plans/plan-1/late-adds/o-v2/resolve", `{"reject":true,"approved_by":"night.dispatch@dealer.com"}`).Code
		return nil
	})
	if code != http.StatusConflict {
		t.Fatalf("rejecting a late add on a held date answered %d, want 409", code)
	}
	if got := d.store.stored("plan-1").LateAdds[0].Status; got != LateAddPending {
		t.Errorf("the refused rejection left the late add %s", got)
	}
}
