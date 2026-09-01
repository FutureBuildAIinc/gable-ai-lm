// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// The whole cross-product, run as races.
//
// The single-writer files each pin one pair against the real handlers. This one
// asserts the property they were each an instance of: for EVERY pair of writers
// to a dispatch date, in either order, the date's board and its ledgers agree
// afterwards. Four writers give ten pairs — the six mixed ones plus each writer
// against another instance of itself, because "two dispatchers both pressing
// Push" is as reachable as "one pushing while another re-plans".
//
// This is the file that would catch a fifth writer being added to
// internal/workflow with a claim that is subtly not the same claim. It is
// probabilistic and it says only "not seen"; the deterministic half of the same
// property is TestEveryWriterToADateIsRefusedWhileTheDateIsHeld.

// pairWriter is one writer, parameterised by which plan it acts on so a pair can
// be given two different plans for the same date and actually contend.
type pairWriter struct {
	name string
	post func(d *dispatchDay, planID string) int
}

var pairWriters = []pairWriter{
	{name: "push", post: func(d *dispatchDay, id string) int {
		return d.post("/api/v1/workflow/plans/"+id+"/push", ``).Code
	}},
	{name: "re-plan", post: func(d *dispatchDay, _ string) int {
		return d.ingest(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`).Code
	}},
	{name: "re-assign", post: func(d *dispatchDay, id string) int {
		return d.post("/api/v1/workflow/plans/"+id+"/assign", `{"override":true,"approved_by":"dispatcher@dealer.com"}`).Code
	}},
	{name: "late-add", post: func(d *dispatchDay, id string) int {
		return d.post("/api/v1/workflow/plans/"+id+"/late-adds/o-v2/resolve", `{"approved_by":"dispatcher@dealer.com"}`).Code
	}},
}

// pairTrial runs ONE trial: two plans for one date claiming the SAME trucks,
// with writer a driving plan-1 and writer b driving plan-2, released together on
// their own goroutines through the real HTTP handlers.
//
// The trucks overlap deliberately. That is the contended case: whatever either
// writer recalls or claims is something the other one also has an opinion about.
func pairTrial(t *testing.T, a, b pairWriter) (violations []string, codes [2]int, d *dispatchDay) {
	t.Helper()
	d = newDispatchDay(t, planWithAQueuedLateAdd(), planWithAQueuedLateAdd())
	d.push("plan-1") // the date is live before either writer starts

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		jitter()
		codes[0] = a.post(d, "plan-1")
	}()
	go func() {
		defer wg.Done()
		<-start
		jitter()
		codes[1] = b.post(d, "plan-2")
	}()
	close(start)
	wg.Wait()
	return d.acceptanceViolations(), codes, d
}

// TestEveryPairOfDateWritersLeavesTheBoardAndItsLedgersAgreeing is acceptance
// III, run 400 times per pair under -race.
//
// Four things are asserted per pair, and dropping any one would let a useless
// implementation pass:
//
//   - zero acceptance violations (the property);
//   - contention was actually REACHED (a run in which the two never met proves
//     nothing about a claim);
//   - no trial refused BOTH writers (a claim that refuses everybody serializes
//     perfectly and dispatches nothing);
//   - BOTH orderings occurred — some trials refused the first writer and some
//     the second — so the zero above is not the zero of a harness that only ever
//     produced one sequence.
func TestEveryPairOfDateWritersLeavesTheBoardAndItsLedgersAgreeing(t *testing.T) {
	const trials = 400

	pairs := 0
	for i := range pairWriters {
		for j := i; j < len(pairWriters); j++ {
			a, b := pairWriters[i], pairWriters[j]
			pairs++
			t.Run(fmt.Sprintf("%s vs %s", a.name, b.name), func(t *testing.T) {
				violated, contended, aRefused, bRefused, bothRan := 0, 0, 0, 0, 0
				var first []string
				for n := 0; n < trials; n++ {
					v, codes, d := pairTrial(t, a, b)
					if len(v) > 0 {
						violated++
						if first == nil {
							first = v
						}
					}
					if _, refusals := d.store.lockCounts(); refusals > 0 {
						contended++
					}
					switch {
					case codes[0] == http.StatusConflict && codes[1] == http.StatusConflict:
						t.Fatalf("trial %d: BOTH writers were refused (%v) — a claim that refuses everybody dispatches nothing", n, codes)
					case codes[0] == http.StatusConflict:
						aRefused++
					case codes[1] == http.StatusConflict:
						bRefused++
					default:
						bothRan++
					}
				}
				if violated != 0 {
					t.Errorf("%d of %d trials violated the acceptance oracle; first was:\n  %v", violated, trials, first)
				}
				if contended == 0 {
					t.Errorf("%d trials and not one contended for the date — these two writers are not taking the same claim", trials)
				}
				if aRefused == 0 && bRefused == 0 {
					t.Errorf("%d trials and neither writer was ever refused — every trial serialized itself by luck", trials)
				}
				t.Logf("%-24s %d trials, %d violations, %d contended, %d/%d refused (first/second), %d ran clean",
					a.name+" vs "+b.name, trials, violated, contended, aRefused, bRefused, bothRan)
			})
		}
	}
	if pairs != 10 {
		t.Fatalf("%d writer pairs, want 10 — a writer was added or removed without this count being reconsidered", pairs)
	}
}
