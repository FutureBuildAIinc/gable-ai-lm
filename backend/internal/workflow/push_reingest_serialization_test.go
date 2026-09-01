// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is the acceptance evidence for the SECOND half of serializing a
// dispatch date: the writers that are not Push.
//
// push_serialization_test.go pins "two pushes for one date cannot both claim a
// truck", and that claim held. It was also the whole of the claim. Ingest's
// supersede and Assign's recall write the same dealer's dispatch board for the
// same date, took no claim on it, and were therefore free to run straight
// through a push — so the date was serialized against exactly one of its three
// writers.
//
// The harm is not a lost update. It is a day with NOTHING on the dealer's
// board and a ledger insisting two trucks are out:
//
//	push=200  ingest=201
//	  plan-2 status=PUSHED  liveClaims=[v1 v2]
//	  plan-1 status=PUSHED  liveClaims=[]
//	  ERP board   = []
//	  ERP recalls = [v1 v2]
//
// The re-plan computes its recall set from a snapshot taken BEFORE the push,
// and executes it AFTER. Both dispatchers are told they succeeded. Nobody is
// told the day is empty. At 128bd17 the trials below reported 74-113 violations
// in 400, with no crash, from two plain concurrent HTTP requests.

// ---------------------------------------------------------------------------
// I. push vs an approved re-plan
// ---------------------------------------------------------------------------

// pushVsReplanTrial runs ONE trial: a date already live on the board, then a
// push of a second plan for the same trucks racing an APPROVED re-plan of the
// same date, both through the real HTTP handlers on their own goroutines.
//
// The two overlap on trucks deliberately. That is the contended case: the
// re-plan's doomed set and the push's claims are the same two vehicles, and the
// race is the interleaving in which the re-plan recalls what the push has just
// written.
func pushVsReplanTrial(t *testing.T) (violations []string, pushCode, ingestCode int, d *dispatchDay) {
	t.Helper()
	d = newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v2"))
	d.push("plan-1")

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start // both goroutines park here, so they are released together
		pushCode = d.post("/api/v1/workflow/plans/plan-2/push", ``).Code
	}()
	go func() {
		defer wg.Done()
		<-start
		ingestCode = d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`).Code
	}()
	close(start)
	wg.Wait()
	return d.acceptanceViolations(), pushCode, ingestCode, d
}

// TestAPushRacingAnApprovedReplanNeverEmptiesTheDay is acceptance I for the
// re-plan writer, run 400 times against the real handlers under -race.
//
// The trial count is part of the result, not decoration: the defect needs one
// specific interleaving of two ERP round-trips, so a single green run is
// indistinguishable from a lucky one.
//
// Four things are asserted, and dropping any one would let a useless
// implementation pass:
//
//   - zero acceptance violations (the property);
//   - contention was actually REACHED (a run in which the two never met proves
//     nothing about a claim);
//   - no trial refused BOTH callers (a claim that refuses everybody serializes
//     perfectly and dispatches nothing);
//   - both orderings occurred across the run — some trials let the push land
//     first and some let the re-plan land first — so the zero above is not the
//     zero of a harness that only ever produced one sequence.
func TestAPushRacingAnApprovedReplanNeverEmptiesTheDay(t *testing.T) {
	const trials = 400

	violated, contended, pushFirst, replanFirst := 0, 0, 0, 0
	var first []string
	for i := 0; i < trials; i++ {
		v, pushCode, ingestCode, d := pushVsReplanTrial(t)
		if len(v) > 0 {
			violated++
			if first == nil {
				first = v
			}
		}
		if _, refusals := d.store.lockCounts(); refusals > 0 {
			contended++
		}
		if pushCode != http.StatusOK && pushCode != http.StatusConflict {
			t.Fatalf("trial %d: the push answered %d — the only legal answers are 200 (it ran) and 409 (the date was held and it did nothing)", i, pushCode)
		}
		if ingestCode != http.StatusCreated && ingestCode != http.StatusConflict {
			t.Fatalf("trial %d: the re-plan answered %d — the only legal answers are 201 (it ran) and 409 (the date was held and it did nothing)", i, ingestCode)
		}
		if pushCode == http.StatusConflict && ingestCode == http.StatusConflict {
			t.Fatalf("trial %d: BOTH writers were refused — a claim that lets nobody through serializes the date by dispatching nothing", i)
		}
		switch {
		case ingestCode == http.StatusConflict:
			pushFirst++
		case pushCode == http.StatusConflict:
			replanFirst++
		default:
			// Neither was refused: they did not overlap in the claimed
			// section. Which of the two ran first is not observable from the
			// codes, and does not need to be — the oracle judges the state.
		}
	}

	if violated != 0 {
		t.Errorf("%d of %d trials violated the acceptance oracle; first was:\n  %v", violated, trials, first)
	}
	if contended == 0 {
		t.Errorf("%d trials and not one contended for the date — this harness never reached the state it exists to test, so its zero proves nothing", trials)
	}
	if pushFirst == 0 && replanFirst == 0 {
		t.Errorf("%d trials and neither writer was ever refused — the two never overlapped, so nothing here was serialized", trials)
	}
	t.Logf("acceptance (push vs approved re-plan): %d trials, %d violations, %d contended (%d push won the date, %d re-plan won it)",
		trials, violated, contended, pushFirst, replanFirst)
}

// TestAPushAndAReplanThatBothRetryLeaveTheBoardAndLedgersAgreeing is the same
// race with the refused caller doing what a dispatcher does with a 409:
// pressing the button again.
//
// It exists because the test above, alone, is weaker than it looks. Under a
// fail-fast claim the loser is refused having done nothing, so that test can be
// satisfied by only ever letting ONE writer through — and the interesting
// state, a date that has been re-planned AND pushed, is never reached. Here
// both really do run, one after the other, and the oracle then has something to
// check.
//
// This is the sequence the claim is supposed to produce. If serialization were
// achieved by losing work rather than by ordering it, this is the test that
// would notice.
func TestAPushAndAReplanThatBothRetryLeaveTheBoardAndLedgersAgreeing(t *testing.T) {
	const trials = 400
	const maxRetries = 20

	violated, retried, bothLanded := 0, 0, 0
	var first []string
	for i := 0; i < trials; i++ {
		d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v2"))
		d.push("plan-1")

		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		landed, sawBusy := 0, false
		note := func(busy bool) {
			mu.Lock()
			defer mu.Unlock()
			if busy {
				sawBusy = true
			} else {
				landed++
			}
		}
		attempt := func(name string, want int, do func() *httpResult) {
			defer wg.Done()
			<-start
			for n := 0; n <= maxRetries; n++ {
				rec := do()
				if rec.code == http.StatusConflict {
					note(true)
					continue // the date was held; nothing was done, so repeat
				}
				if rec.code == want {
					note(false)
				} else {
					t.Errorf("trial %d: %s answered %d (%s)", i, name, rec.code, rec.body)
				}
				return
			}
			t.Errorf("trial %d: %s never got the date in %d attempts", i, name, maxRetries)
		}
		wg.Add(2)
		go attempt("push", http.StatusOK, func() *httpResult {
			return result(d.post("/api/v1/workflow/plans/plan-2/push", ``))
		})
		go attempt("re-plan", http.StatusCreated, func() *httpResult {
			return result(d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`))
		})
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
		t.Errorf("%d trials and none was ever refused for a held date — the retry path was never exercised", trials)
	}
	if bothLanded != trials {
		t.Errorf("only %d of %d trials got BOTH writers through; serialization must order the two, not drop one", bothLanded, trials)
	}
	t.Logf("acceptance (push + re-plan, both retrying): %d trials, %d violations, %d contended, %d with both writers through",
		trials, violated, retried, bothLanded)
}

// TestAReplanAfterAPushIsCleanWithoutTheRace is the sequential control for the
// trials above.
//
// It is what makes the 400-trial numbers mean "the interleaving", rather than
// "these two operations are broken together in any order". Run one after the
// other, the same two calls leave the board and the ledgers agreeing every
// time — which is exactly why the concurrent version was invisible to every
// non-concurrent test in this package.
func TestAReplanAfterAPushIsCleanWithoutTheRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v2"))
		d.push("plan-1")
		if rec := d.post("/api/v1/workflow/plans/plan-2/push", ``); rec.Code != http.StatusOK {
			t.Fatalf("push plan-2: %d %s", rec.Code, rec.Body.String())
		}
		if rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`); rec.Code != http.StatusCreated {
			t.Fatalf("re-plan: %d %s", rec.Code, rec.Body.String())
		}
		if v := d.acceptanceViolations(); len(v) > 0 {
			t.Fatalf("trial %d: sequential push-then-re-plan violated the oracle: %v", i, v)
		}
		if got := d.g.pushedIDs(); len(got) != 0 {
			t.Fatalf("trial %d: an approved re-plan keeps nothing, board holds %v", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// II. push vs a re-assignment's recall
// ---------------------------------------------------------------------------

// reassignVsPushTrial runs ONE trial of the THIRD writer: a re-assignment that
// drops a truck, racing a push that claims that same truck.
//
// The setup is the only fiddly part. plan-1 holds three trucks and is live on
// all three; the fleet then shrinks to two, so re-assigning plan-1 drops v3 and
// recalls it. plan-2 pushes v3 at the same instant. The recall is keyed
// (vehicle_id, scheduled_date) — not "the route this plan pushed" — so a
// re-assignment that decided v3 was doomed BEFORE plan-2 wrote it cancels
// plan-2's live route and leaves plan-2's ledger claiming a truck the dealer's
// board does not hold.
func reassignVsPushTrial(t *testing.T) (violations []string, assignCode, pushCode int, d *dispatchDay) {
	t.Helper()
	d = newDispatchDay(t, planForTrucks("v1", "v2", "v3"), planForTrucks("v3"))
	d.push("plan-1")

	// The yard puts a truck out of service: re-assigning plan-1 now has to drop
	// v3, which is what makes this a board WRITE and not just a re-shuffle.
	d.g.mu.Lock()
	d.g.vehicles = d.g.vehicles[:2]
	d.g.mu.Unlock()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		jitter()
		assignCode = d.post("/api/v1/workflow/plans/plan-1/assign", `{"override":true,"approved_by":"dispatcher@dealer.com"}`).Code
	}()
	go func() {
		defer wg.Done()
		<-start
		jitter()
		pushCode = d.post("/api/v1/workflow/plans/plan-2/push", ``).Code
	}()
	close(start)
	wg.Wait()
	return d.acceptanceViolations(), assignCode, pushCode, d
}

// jitter staggers two racing request goroutines by a random sub-millisecond
// amount.
//
// Releasing both from one channel is not enough here, and pretending it is
// would be the more dangerous kind of green. These two endpoints do measurably
// different amounts of work before they reach the contended section — the
// re-assignment decodes a JSON body, the push does not — and without a stagger
// the push won the date in 400 trials out of 400. A harness that only ever
// produces one ordering cannot see a defect that lives in the other one.
func jitter() {
	time.Sleep(time.Duration(rand.Intn(400)) * time.Microsecond)
}

// TestAReassignmentRacingAPushNeverCancelsTheRouteItJustWrote is acceptance I
// for the assign writer, run 400 times under -race.
func TestAReassignmentRacingAPushNeverCancelsTheRouteItJustWrote(t *testing.T) {
	const trials = 400

	violated, contended, dropped := 0, 0, 0
	var first []string
	for i := 0; i < trials; i++ {
		v, assignCode, pushCode, d := reassignVsPushTrial(t)
		if len(v) > 0 {
			violated++
			if first == nil {
				first = v
			}
		}
		if _, refusals := d.store.lockCounts(); refusals > 0 {
			contended++
		}
		if assignCode != http.StatusOK && assignCode != http.StatusConflict {
			t.Fatalf("trial %d: the re-assignment answered %d — the only legal answers are 200 and 409", i, assignCode)
		}
		if pushCode != http.StatusOK && pushCode != http.StatusConflict {
			t.Fatalf("trial %d: the push answered %d — the only legal answers are 200 and 409", i, pushCode)
		}
		if assignCode == http.StatusConflict && pushCode == http.StatusConflict {
			t.Fatalf("trial %d: BOTH writers were refused", i)
		}
		if assignCode == http.StatusOK {
			dropped++
		}
	}

	if violated != 0 {
		t.Errorf("%d of %d trials violated the acceptance oracle; first was:\n  %v", violated, trials, first)
	}
	if contended == 0 {
		t.Errorf("%d trials and not one contended for the date — the re-assignment is not taking the same claim the push takes", trials)
	}
	if dropped == 0 {
		t.Errorf("%d trials and the re-assignment never once ran — this harness proves nothing about its recall", trials)
	}
	t.Logf("acceptance (re-assignment vs push): %d trials, %d violations, %d contended, %d re-assignments ran", trials, violated, contended, dropped)
}

// ---------------------------------------------------------------------------
// III. every writer takes the SAME claim, and says so
// ---------------------------------------------------------------------------

// dateWriters is every HTTP call that writes this date's dispatch board or the
// ledger mirroring it. The table is the audit: a path added here that does not
// appear below is a path that can run through somebody else's claim.
//
// Pack, Resequence, SetPriority, SetLineDimensions, the lock/unlock pair, the
// late-add pair and the proof/sign-off pair are deliberately ABSENT: none of
// them recalls a route, pushes one, or writes a LiveRoute entry, so none of
// them can put this date's board and its ledgers out of step. They are guarded
// by the plan's optimistic version, which is the right tool for "two people
// editing one plan" and the wrong one for "two people editing one DATE".
var dateWriters = []struct {
	name string
	path string
	body string
	want int // the status when the claim is available
}{
	{name: "push", path: "/api/v1/workflow/plans/plan-1/push", body: ``, want: http.StatusOK},
	{name: "re-plan (ingest)", path: "/api/v1/workflow/plans", body: `{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`, want: http.StatusCreated},
	{name: "re-assign", path: "/api/v1/workflow/plans/plan-1/assign", body: `{"override":true,"approved_by":"dispatcher@dealer.com"}`, want: http.StatusOK},
}

// TestEveryWriterToADateIsRefusedWhileTheDateIsHeld is the deterministic
// statement of the fix, and the one that fails the moment a writer is dropped
// from the claim.
//
// The 400-trial races above are probabilistic and can only ever say "not seen".
// This says "cannot": each writer is invoked while the date is held by somebody
// else, and each must answer 409 having sent NOTHING to GableLBM and written
// NOTHING to the store — so that "try again" is a plain repeat and never a
// partial recovery.
func TestEveryWriterToADateIsRefusedWhileTheDateIsHeld(t *testing.T) {
	for _, w := range dateWriters {
		t.Run(w.name, func(t *testing.T) {
			d := newDispatchDay(t, planForTrucks("v1", "v2"))
			d.push("plan-1") // the date is live, so every writer below has real work to refuse

			var (
				code   int
				body   string
				calls  int
				saves  int
				plans  int
				recall int
			)
			err := d.store.WithDateLock(context.Background(), d.date, func(context.Context) error {
				rec := d.post(w.path, w.body)
				code, body = rec.Code, rec.Body.String()
				d.g.mu.Lock()
				calls, recall = d.g.pushCalls, len(d.g.recalled)
				d.g.mu.Unlock()
				d.store.mu.Lock()
				saves = d.store.updates
				d.store.mu.Unlock()
				plans = d.store.count()
				return nil
			})
			if err != nil {
				t.Fatalf("holding the date should succeed: %v", err)
			}

			if code != http.StatusConflict {
				t.Fatalf("%s while the date is held answered %d, want 409 Conflict — %s", w.name, code, body)
			}
			// The setup push made two calls and one save; anything beyond that
			// is work the refusal did before refusing.
			if calls != 2 {
				t.Errorf("the refused %s reached GableLBM's push %d time(s) (want the 2 from setup) — a fail-fast claim that has already written upstream is not fail-fast", w.name, calls)
			}
			if recall != 0 {
				t.Errorf("the refused %s recalled %d route(s) — it took work off the dealer's board before refusing", w.name, recall)
			}
			if saves != 1 {
				t.Errorf("the refused %s wrote the plan store %d time(s) (want the 1 from setup) — it must leave nothing behind to reconcile", w.name, saves)
			}
			if plans != 1 {
				t.Errorf("the refused %s left %d plans stored, want 1", w.name, plans)
			}

			d.assertAcceptance()
		})
	}
}

// TestADateBusyRefusalTellsTheDispatcherSomethingTrue pins the copy, because
// the whole justification for failing fast is that the person refused is told
// something they can act on.
//
// Every clause is load-bearing and every one of them was false in what shipped
// before: the app rendered ONE hardcoded banner for every 409 — "Someone else
// changed this plan while you were working. Your change was not applied. Reload
// to see theirs, then redo yours." — which for a held date names no date,
// blames a change nobody made, and prescribes a reload that fixes nothing.
//
//   - the DATE is named, because a dispatcher has several open and "try again"
//     is useless if they do not know which day is shut;
//   - it says what did NOT happen, in that writer's own words;
//   - it promises nothing reached GableLBM, which is what makes a plain repeat
//     safe;
//   - it carries the DATE_BUSY code, so the client can tell this 409 from the
//     version-conflict 409 without matching prose.
func TestADateBusyRefusalTellsTheDispatcherSomethingTrue(t *testing.T) {
	wantPhrase := map[string]string{
		"push":             "this push did not run",
		"re-plan (ingest)": "the day was not re-planned",
		"re-assign":        "the trucks were not re-assigned",
	}
	for _, w := range dateWriters {
		t.Run(w.name, func(t *testing.T) {
			d := newDispatchDay(t, planForTrucks("v1", "v2"))
			d.push("plan-1")

			var body []byte
			var code int
			_ = d.store.WithDateLock(context.Background(), d.date, func(context.Context) error {
				rec := d.post(w.path, w.body)
				code, body = rec.Code, rec.Body.Bytes()
				return nil
			})
			if code != http.StatusConflict {
				t.Fatalf("want 409, got %d (%s)", code, body)
			}

			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("the refusal must be the standard error envelope: %v (%s)", err, body)
			}
			if env.Error.Code != CodeDateBusy {
				t.Errorf("code %q, want %q — a client that cannot tell this 409 from a version conflict shows the wrong banner and the wrong button", env.Error.Code, CodeDateBusy)
			}
			msg := env.Error.Message
			if !strings.Contains(msg, d.date) {
				t.Errorf("the refusal must name the date that is shut, got %q", msg)
			}
			if !strings.Contains(msg, wantPhrase[w.name]) {
				t.Errorf("the refusal must say what did not happen (%q), got %q", wantPhrase[w.name], msg)
			}
			for _, want := range []string{"Nothing was sent to GableLBM", "try again"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal must tell the dispatcher %q; got %q", want, msg)
				}
			}
			// The module's own vocabulary must not reach the yard.
			if strings.Contains(msg, "dispatch date busy") {
				t.Errorf("the sentinel's wording leaked into the operator's sentence: %q", msg)
			}
		})
	}
}

// httpResult is a recorder reduced to the two fields the retry loops read.
type httpResult struct {
	code int
	body string
}

func result(rec *httptest.ResponseRecorder) *httpResult {
	return &httpResult{code: rec.Code, body: rec.Body.String()}
}

// TestTheReplansRecallSetIsReadUnderTheClaim closes the one hole the 400-trial
// races above cannot reliably see.
//
// A re-plan reads which plans hold the date, and several steps later recalls
// exactly what that read named. Taking that read just BEFORE the claim instead
// of inside it converges in every ordinary ordering and is invisible to every
// end-state assertion — and it is the entire defect: at 128bd17 the read sat
// three ERP round-trips ahead of the recall, and a push landing in that window
// was recalled off the board by a re-plan that had never seen it.
//
// So a competing push is landed in precisely that gap, synchronously, and the
// question is only whether the claim refuses it. It must: the recall set the
// re-plan is about to execute was decided under the claim, and nothing may
// change the board between the deciding and the doing.
func TestTheReplansRecallSetIsReadUnderTheClaim(t *testing.T) {
	d := newDispatchDay(t, planForTrucks("v1", "v2"), planForTrucks("v1", "v2"))
	d.push("plan-1")

	// Ingest reads the date twice: a cheap pre-ERP gate, then the
	// authoritative read the recall set comes from. The competing push goes
	// immediately after the second one.
	base := d.store.lists()
	var competing int
	d.store.afterListForDate = func(n int) {
		if n != base+2 {
			return
		}
		d.store.afterListForDate = nil
		competing = d.post("/api/v1/workflow/plans/plan-2/push", ``).Code
	}

	rec := d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)

	if competing == 0 {
		t.Fatal("the competing push never ran — this test asserted nothing")
	}
	if competing != http.StatusConflict {
		t.Errorf("a push landing between the re-plan's read and its recall answered %d, want 409 — the recall set was decided outside the claim and can be executed against a board that moved", competing)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("the re-plan itself must still proceed: %d %s", rec.Code, rec.Body.String())
	}
	if got := d.g.pushedIDs(); len(got) != 0 {
		t.Errorf("the board holds %v after an approved re-plan, want nothing", sorted(got))
	}
	d.assertAcceptance()
}
