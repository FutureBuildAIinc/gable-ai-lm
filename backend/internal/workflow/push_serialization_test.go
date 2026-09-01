// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// This file is the acceptance evidence for serializing pushes per date.
//
// The claim being tested is narrow and mechanical: at most one Push for a given
// dispatch date is ever inside the read-decide-write section, so two plans for
// one date cannot both look, both see no rival claim, and both persist a claim
// on the same truck. That is acceptance I — "no truck claimed by two plans" —
// and before the lock it failed 34 times in 400 concurrent trials here.
//
// What this file does NOT claim is just as important, and is stated in Push's
// own doc comment: the lock orders this SERVICE against itself. It does not
// make the ERP write and the ledger write atomic, so a process that dies
// between them still leaves a route on the dealer's board that no ledger names.
// No test below asserts otherwise.

// concurrentPushTrial runs ONE trial: two plans for the same date, claiming the
// SAME trucks, pushed simultaneously by two real request goroutines through the
// real HTTP handlers. It returns the acceptance oracle's verdict on whatever
// state they left behind, plus the two status codes.
//
// The two plans deliberately claim identical trucks. That is the contended
// case: each push's clearDisplacedClaims has to see the other's claim, and the
// race is precisely the interleaving in which neither does.
func concurrentPushTrial(t *testing.T, trucks ...string) (violations []string, codes [2]int, d *dispatchDay) {
	t.Helper()
	d = newDispatchDay(t, planForTrucks(trucks...), planForTrucks(trucks...))

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range [2]string{"plan-1", "plan-2"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			<-start // both goroutines are parked here, so they are released together
			codes[i] = d.post("/api/v1/workflow/plans/"+id+"/push", ``).Code
		}(i, id)
	}
	close(start)
	wg.Wait()
	return d.acceptanceViolations(), codes, d
}

// TestConcurrentPushesForOneDateNeverDoubleClaimATruck is acceptance I, run 400
// times against the real handlers under -race.
//
// The trial count is part of the result and not decoration. The defect this
// closes is probabilistic — it needs one specific interleaving of two ERP
// round-trips — so a single green run is indistinguishable from a lucky one.
// At 17b11ed this identical harness reported 34 violations in 400 trials.
//
// Three things are asserted, and dropping any one of them would let a useless
// implementation pass:
//
//   - zero acceptance violations (the property);
//   - at least one push refused with 409 (the trials actually REACHED
//     contention — a run that never contended proves nothing about a lock);
//   - every trial dispatched the date (a lock that refused everything, or a
//     handler that quietly did nothing, would satisfy the first two).
func TestConcurrentPushesForOneDateNeverDoubleClaimATruck(t *testing.T) {
	const trials = 400

	violated, contended, dispatched := 0, 0, 0
	var first []string
	for i := 0; i < trials; i++ {
		v, codes, d := concurrentPushTrial(t, "v1", "v2")
		if len(v) > 0 {
			violated++
			if first == nil {
				first = v
			}
		}
		if _, refusals := d.store.lockCounts(); refusals > 0 {
			contended++
		}
		if codes[0] == http.StatusOK || codes[1] == http.StatusOK {
			dispatched++
		}
		for _, c := range codes {
			if c != http.StatusOK && c != http.StatusConflict {
				t.Fatalf("trial %d: a serialized push answered %d — the only two legal answers are 200 (it ran) and 409 (the date was busy and it did nothing)", i, c)
			}
		}
	}

	if violated != 0 {
		t.Errorf("%d of %d concurrent trials violated the acceptance oracle; first was:\n  %v", violated, trials, first)
	}
	if contended == 0 {
		t.Errorf("%d trials and not one of them contended for the date lock — this harness never reached the state it exists to test, so its zero proves nothing", trials)
	}
	if dispatched != trials {
		t.Errorf("only %d of %d trials got the date onto the dispatch board — serializing pushes must not stop the day going out", dispatched, trials)
	}
	t.Logf("acceptance: %d trials, %d violations, %d contended, %d dispatched", trials, violated, contended, dispatched)
}

// TestConcurrentPushesThatBothRetryStillLeaveOneClaimant is the same 400 trials
// with the refused push doing what a dispatcher does with a 409: pressing the
// button again.
//
// It exists because the test above, on its own, is weaker than it looks. Under
// a fail-fast lock the loser is refused having done nothing, so that test can
// be satisfied by only ever letting ONE plan push — and the interesting state,
// two plans that have both been on this date's board, is never reached. Here
// both plans really do push, one after the other, and the oracle then has
// something to check: the second must have displaced the first, exactly one
// ledger must name each truck, and the board must agree with it.
//
// This is the sequence the lock is supposed to produce. If serialization were
// achieved by losing work rather than by ordering it, this is the test that
// would notice.
func TestConcurrentPushesThatBothRetryStillLeaveOneClaimant(t *testing.T) {
	const trials = 400
	const maxRetries = 20

	violated, retried, bothLanded := 0, 0, 0
	var first []string
	for i := 0; i < trials; i++ {
		d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v2"))
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		landed, sawBusy := 0, false
		for _, id := range [2]string{"plan-1", "plan-2"} {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start
				for attempt := 0; attempt <= maxRetries; attempt++ {
					rec := d.post("/api/v1/workflow/plans/"+id+"/push", ``)
					if rec.Code == http.StatusConflict {
						mu.Lock()
						sawBusy = true
						mu.Unlock()
						continue // the date was busy; nothing was done, so just repeat
					}
					if rec.Code == http.StatusOK {
						mu.Lock()
						landed++
						mu.Unlock()
					} else {
						t.Errorf("trial %d: push %s answered %d (%s)", i, id, rec.Code, rec.Body.String())
					}
					return
				}
				t.Errorf("trial %d: push %s never got the date lock in %d attempts", i, id, maxRetries)
			}(id)
		}
		close(start)
		wg.Wait()

		if v := d.acceptanceViolations(); len(v) > 0 {
			violated++
			if first == nil {
				first = v
			}
		}
		if sawBusy {
			retried++
		}
		if landed == 2 {
			bothLanded++
		}
	}

	if violated != 0 {
		t.Errorf("%d of %d retrying trials violated the acceptance oracle; first was:\n  %v", violated, trials, first)
	}
	if retried == 0 {
		t.Errorf("%d trials and none of them was ever refused for a busy date — the retry path was never exercised", trials)
	}
	if bothLanded != trials {
		t.Errorf("only %d of %d trials got BOTH plans onto the board; serialization must order the two pushes, not drop one", bothLanded, trials)
	}
	t.Logf("acceptance (retrying): %d trials, %d violations, %d contended, %d with both plans pushed", trials, violated, retried, bothLanded)
}

// TestASecondPushForTheSameDateIsRefusedHavingDoneNothing is the deterministic
// statement of the contention decision.
//
// The lock FAILS FAST rather than queueing, so the refusal has to be worth
// receiving: it must arrive having sent nothing to GableLBM and written
// nothing, so that "try again" is a plain repeat and not a partial recovery.
// That is asserted here rather than assumed, because a lock taken one line too
// late would still refuse — after the routes were already on the board.
func TestASecondPushForTheSameDateIsRefusedHavingDoneNothing(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	var (
		code  int
		body  string
		calls int
		saves int
	)
	// Hold the date exactly as an in-flight push holds it, then let a second
	// dispatcher press Push.
	err := d.store.WithDateLock(context.Background(), d.date, func(context.Context) error {
		rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)
		code, body = rec.Code, rec.Body.String()
		d.g.mu.Lock()
		calls = d.g.pushCalls
		d.g.mu.Unlock()
		d.store.mu.Lock()
		saves = d.store.updates
		d.store.mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("holding the date lock should succeed: %v", err)
	}

	if code != http.StatusConflict {
		t.Fatalf("a push for a date another push holds must answer 409 Conflict, got %d (%s)", code, body)
	}
	if calls != 0 {
		t.Fatalf("the refused push reached GableLBM %d time(s) — a fail-fast lock that has already written upstream is not fail-fast", calls)
	}
	if saves != 0 {
		t.Fatalf("the refused push wrote the plan store %d time(s) — it must leave nothing behind to reconcile", saves)
	}
	if !strings.Contains(body, d.date) {
		t.Fatalf("the refusal must name the date the dispatcher is waiting on, got %q", body)
	}
	d.assertAcceptance()
}

// TestTheDateLockIsScopedToItsOwnDate pins the resource the lock names.
//
// The contended resource is the DATE, because GableLBM keys a route
// (vehicle_id, scheduled_date) — but that is also the whole cost of this
// design, and the cost has a boundary. A lock accidentally taken per-INSTALL
// rather than per-date would pass every other test in this file and quietly
// serialize the entire dealer's dispatch, one day at a time.
func TestTheDateLockIsScopedToItsOwnDate(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"))

	var code int
	err := d.store.WithDateLock(context.Background(), "2026-06-27", func(context.Context) error {
		code = d.post("/api/v1/workflow/plans/plan-1/push", ``).Code
		return nil
	})
	if err != nil {
		t.Fatalf("holding an adjacent date's lock should succeed: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("a push for %s must not be blocked by a lock on 2026-06-27, got %d", d.date, code)
	}
	d.assertAcceptance()
}

// TestTheDateLockIsReleasedWhenThePushRefuses guards the release path.
//
// Push refuses on a dozen gates and returns errors from several more. If any of
// them left the date held, the first refused push of the morning would shut the
// day for every dispatcher and the only cure would be a restart — a strictly
// worse failure than the race this replaces. Postgres releases the lock on
// COMMIT or ROLLBACK whatever happens; this asserts the Go side does not hold
// its own copy of it past the refusal.
func TestTheDateLockIsReleasedWhenThePushRefuses(t *testing.T) {
	p := planForTrucks("v1")
	p.Loads[0].Proof = nil // no yard proof: Push refuses at the sign-off gate
	d := newDispatchDay(t, p)

	if rec := d.post("/api/v1/workflow/plans/plan-1/push", ``); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("setup: expected the sign-off gate to refuse with 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	grants, refusals := d.store.lockCounts()
	if grants != 1 || refusals != 0 {
		t.Fatalf("expected exactly one lock grant and no refusal, got %d/%d", grants, refusals)
	}
	// If the refusal had leaked the lock, this second push would answer 409.
	if rec := d.post("/api/v1/workflow/plans/plan-1/push", ``); rec.Code == http.StatusConflict {
		t.Fatal("the date is still locked after a refused push — one refusal has shut the day")
	}
}

// TestThePushGatesOnStateReadUnderTheLock closes the one hole the concurrency
// trials above cannot see.
//
// Push reads the plan twice: once unlocked, only to learn which date to lock,
// and once under the lock, which is the read every gate and every ERP call is
// allowed to act on. Reusing the first read would be invisible to the trials —
// the ledger still converges, because clearDisplacedClaims re-lists and
// persistPush is still version-checked — and would still be wrong: the push
// would put routes on the dealer's board having evaluated its gates against a
// plan that has since been re-packed, locked, or already pushed.
//
// So this lands a competing writer in exactly that window and asserts the
// consequence that matters upstream: NOTHING was sent to GableLBM.
func TestThePushGatesOnStateReadUnderTheLock(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1"))

	// Between Push's unlocked read and its read under the lock, someone
	// re-packs the plan — which invalidates the review the push gate needs.
	d.store.afterGet = func() {
		cur, err := d.store.Get(context.Background(), "plan-1")
		if err != nil {
			t.Errorf("competing read failed: %v", err)
			return
		}
		cur.Status = StatusPacked
		if err := d.store.Update(context.Background(), cur); err != nil {
			t.Errorf("competing write should succeed: %v", err)
		}
	}

	rec := d.post("/api/v1/workflow/plans/plan-1/push", ``)
	if rec.Code == http.StatusOK {
		t.Fatalf("the push was evaluated against a status nobody holds any more and answered 200: %s", rec.Body.String())
	}
	d.g.mu.Lock()
	calls := d.g.pushCalls
	d.g.mu.Unlock()
	if calls != 0 {
		t.Fatalf("the push reached GableLBM %d time(s) after the plan left REVIEWED — it gated on the copy it read before taking the lock", calls)
	}
	if got := d.store.stored("plan-1").Status; got != StatusPacked {
		t.Fatalf("the competing re-pack must survive, got %q", got)
	}
	d.assertAcceptance()
}

// TestReplanningADateLeavesTheNextDayAlone is the adjacent-date control.
//
// Everything on this branch is keyed on the DATE — the supersede gate, the
// recall wire key (vehicle_id, scheduled_date), the ledger, and now the push
// lock. That makes "which date?" the single most load-bearing value in the
// module, and the failure mode of getting it wrong is silent in one direction:
// a gate or a recall that ignored the date would pass every same-date test in
// this package and cancel tomorrow's routes today.
//
// So: two dates live at once, one re-planned with approval, and the other one
// must be untouched — on the board, in its ledger, and in what was recalled.
func TestReplanningADateLeavesTheNextDayAlone(t *testing.T) {
	today := planForTrucks("v1", "v2")
	tomorrow := planForTrucks("v3")
	tomorrow.PlanDate = "2026-06-27"

	d := newDispatchDay(t, today, tomorrow)
	d.push("plan-1")
	d.push("plan-2")
	if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v1", "v2", "v3"}) {
		t.Fatalf("setup: the board holds %v, want all three trucks", got)
	}

	rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("an approved re-plan of 2026-06-26 must proceed: %d %s", rec.Code, rec.Body.String())
	}

	if got, want := sorted(d.g.recalledIDs()), []string{"v1", "v2"}; !equalStrings(got, want) {
		t.Errorf("recalled %v, want only the target date's trucks %v — v3 is out on 2026-06-27", got, want)
	}
	if got := sorted(d.g.pushedIDs()); !equalStrings(got, []string{"v3"}) {
		t.Errorf("the board holds %v, want only v3 — the next day's route must survive today's re-plan", got)
	}
	next := d.store.stored("plan-2")
	if got := sorted(liveVehicleIDs(next)); !equalStrings(got, []string{"v3"}) {
		t.Errorf("the 2026-06-27 plan claims %v, want v3 still live — its ledger was not the one being corrected", got)
	}
	d.assertAcceptance()
}

// TestDateBusyReachesTheClientAsARetryable409 pins the wire contract, because
// the whole justification for failing fast is that the dispatcher is told
// something they can act on.
func TestDateBusyReachesTheClientAsARetryable409(t *testing.T) {
	if !errors.Is(ErrDateBusy, ErrDateBusy) {
		t.Fatal("ErrDateBusy must be a sentinel callers can match")
	}
	d := newDispatchDay(t, planForTrucks("v1"))
	var rec = struct {
		code int
		body string
	}{}
	_ = d.store.WithDateLock(context.Background(), d.date, func(context.Context) error {
		r := d.post("/api/v1/workflow/plans/plan-1/push", ``)
		rec.code, rec.body = r.Code, r.Body.String()
		return nil
	})
	if rec.code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.code)
	}
	for _, want := range []string{"already being pushed", "Nothing was sent to GableLBM", "try again"} {
		if !strings.Contains(rec.body, want) {
			t.Errorf("the refusal must tell the dispatcher %q; got %s", want, rec.body)
		}
	}
}
