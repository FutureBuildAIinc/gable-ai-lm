// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

// The plan state machine, stated once.
//
// Before this file the machine existed only as behaviour: eight scattered
// `p.Status = ...` assignments, three ad-hoc `if p.Status == StatusReviewed ||
// p.Status == StatusPushed` walk-backs, and — the defect this file was written
// for — no status precondition at all on Assign, Pack or Push. A PUSHED plan
// could be silently re-assigned: Plan.Loads was rebuilt from scratch, the
// LoadPlan/Compliance/Proof artifacts thrown away, and any truck the new
// assignment DROPPED kept a live route on the dealer's dispatch board forever.
// The yard would load a truck for a run that no longer existed.
//
// Every transition now consults one table. What the table encodes:
//
//	                 ANALYZED  ASSIGNED  PACKED  REVIEWED  PUSHED
//	assign              ok        ok       ok       ok     approval + recall
//	pack                 -        ok       ok       ok     approval
//	resequence           -        ok       ok       ok     approval
//	priority            ok        ok       ok       ok     approval
//	dimensions          ok        ok       ok       ok     approval
//	review               -         -       ok       ok     refused
//	push                 -         -        -       ok     refused
//
// Three kinds of cell, and the difference between them is the product decision:
//
//   - ok — the transition runs.
//   - approval — it is refused by default and runs only with an explicit
//     override plus an approver, mapped to HTTP 423 exactly like the T2-3 lock.
//     This is not a second override idiom; it is the same one, because a
//     dispatcher should not have to learn two ways to say "yes, I mean it".
//   - refused — there is nothing to approve. Review and Push on a plan that is
//     already on the board are answered with a Refusal (422) telling the
//     dispatcher to change something first, which walks the plan back to PACKED
//     of its own accord. An override here would only mean "do the same thing
//     twice", so offering one would be dishonest.
//
// An illegal transition writes NOTHING. Every caller reads a copy of the plan,
// gates it, and only then mutates and persists, so a refusal leaves the stored
// plan byte-identical.

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrPushed signals that a change was refused because this plan's routes are
// live on GableLBM's dispatch board and the change would invalidate them.
//
// It is a sibling of ErrLocked, not a replacement: a run can be locked, pushed,
// both or neither, and the two gates answer different questions ("is this run
// frozen for re-optimization?" vs "is the dealer already acting on this?").
// Handlers map both to 423 Locked so the UI prompts for the same approval.
var ErrPushed = errors.New("plan routes are live on the dispatch board")

// Workflow actions. These names key the transition table and supply the verb in
// every refusal it produces, so the vocabulary a dispatcher reads and the
// vocabulary the code branches on are the same one.
const (
	actionAssign     = "assign"
	actionPack       = "pack"
	actionResequence = "resequence"
	actionPriority   = "priority"
	actionDimensions = "dimensions"
	actionReview     = "review"
	actionPush       = "push"
)

// allowance is one cell of the transition table.
type allowance int

const (
	// notListed is the zero value on purpose: a status with no entry for an
	// action is illegal. New statuses therefore fail closed.
	notListed allowance = iota
	allowed
	needsApproval
)

// transition is one row of the table.
type transition struct {
	// verb completes "cannot ..." in a refusal ("cannot pack the trucks").
	verb string
	// gerund completes "... requires manual approval" and is also the action
	// string handed to gateReshuffle, so the lock gate and this gate describe
	// the same operation in the same words.
	gerund string
	// prereq names what must happen first when the plan has not come far
	// enough ("run assign first"). Empty for actions legal from ANALYZED.
	prereq string
	// to is the status a successful transition writes, for the steps that
	// write a fixed one. Empty for the reshuffle actions, which walk a
	// later-stage plan back to PACKED instead (see walkBackAfterReshuffle).
	to   string
	from map[string]allowance
}

// planTransitions is THE table. Nothing in this package may branch on
// p.Status to decide whether an operation is permitted; it asks here.
var planTransitions = map[string]transition{
	actionAssign: {
		verb: "assign trucks", gerund: "re-assigning trucks", to: StatusAssigned,
		from: map[string]allowance{
			StatusAnalyzed: allowed,
			StatusAssigned: allowed,
			StatusPacked:   allowed,
			StatusReviewed: allowed,
			// The defect. Re-assignment is the only transition that changes
			// which trucks exist, so it is the only one that can orphan a
			// route — and therefore the only one that recalls.
			StatusPushed: needsApproval,
		},
	},
	actionPack: {
		verb: "pack the trucks", gerund: "re-packing trucks",
		prereq: "run assign first", to: StatusPacked,
		from: map[string]allowance{
			StatusAssigned: allowed,
			StatusPacked:   allowed,
			StatusReviewed: allowed,
			StatusPushed:   needsApproval,
		},
	},
	actionResequence: {
		verb: "re-sequence a route", gerund: "re-sequencing a route",
		prereq: "run assign first",
		from: map[string]allowance{
			StatusAssigned: allowed,
			StatusPacked:   allowed,
			StatusReviewed: allowed,
			StatusPushed:   needsApproval,
		},
	},
	actionPriority: {
		verb: "change delivery priority", gerund: "changing delivery priority",
		from: map[string]allowance{
			StatusAnalyzed: allowed,
			StatusAssigned: allowed,
			StatusPacked:   allowed,
			StatusReviewed: allowed,
			StatusPushed:   needsApproval,
		},
	},
	actionDimensions: {
		verb: "override line dimensions", gerund: "overriding line dimensions",
		from: map[string]allowance{
			StatusAnalyzed: allowed,
			StatusAssigned: allowed,
			StatusPacked:   allowed,
			StatusReviewed: allowed,
			StatusPushed:   needsApproval,
		},
	},
	actionReview: {
		verb: "review the routes", gerund: "re-running route review",
		prereq: "run pack first", to: StatusReviewed,
		from: map[string]allowance{
			StatusPacked:   allowed,
			StatusReviewed: allowed,
			// PUSHED is absent, deliberately. Review annotates and rebalances;
			// it never changes which trucks exist, so there is no route to
			// recall and nothing for an approver to weigh. Changing the plan
			// walks it back to PACKED, and review is legal again immediately.
		},
	},
	actionPush: {
		verb: "push to the dispatch board", gerund: "re-pushing the run",
		prereq: "run review first", to: StatusPushed,
		from: map[string]allowance{
			StatusReviewed: allowed,
			// PUSHED is absent. A plan already on the board has nothing to
			// send; the answer is "change something first", not "approve".
			//
			// Note this is NOT the resume path. A push that failed part-way
			// leaves the status at REVIEWED precisely so re-running it is a
			// legal, ordinary push that skips the trucks already acked.
		},
	},
}

// liveRoutes returns the routes this plan still believes are on the dispatch
// board (tombstoned recalls excluded).
func liveRoutes(p *Plan) []LiveRoute {
	out := make([]LiveRoute, 0, len(p.LiveRoutes))
	for _, r := range p.LiveRoutes {
		if r.Live() {
			out = append(out, r)
		}
	}
	return out
}

// liveRouteNames lists the trucks whose routes are live, for a refusal message.
func liveRouteNames(live []LiveRoute) string {
	names := make([]string, 0, len(live))
	for _, r := range live {
		if r.VehicleName != "" {
			names = append(names, r.VehicleName)
			continue
		}
		names = append(names, r.VehicleID)
	}
	return strings.Join(names, ", ")
}

// gateTransition is the single precondition every workflow mutation runs.
//
// It answers three questions in order: is this action legal from the plan's
// current status; do live routes escalate an otherwise-legal action to one
// needing approval; and — when it does — was that approval given. On an
// exercised override it records the approver on the plan, the way gateReshuffle
// records one on the lock.
//
// It mutates only p.PushedOverrides, and only on the success path. Every
// refusal returns before the caller has written anything.
func gateTransition(p *Plan, action string, override bool, approvedBy string) error {
	t, ok := planTransitions[action]
	if !ok {
		// A programming error, not a dispatcher's: an action with no row is a
		// transition nobody declared. Fail closed rather than run ungated.
		return fmt.Errorf("no transition rule declared for workflow action %q", action)
	}

	allow := t.from[p.Status]

	// Live routes escalate. A push that failed on truck 3 of 5 leaves the plan
	// at REVIEWED with trucks 1-2 live upstream, and re-assigning THAT plan
	// orphans exactly as much as re-assigning a fully PUSHED one. Status alone
	// would miss it. Only actions the table already marks approval-worthy from
	// PUSHED escalate, so the table stays the single source of truth.
	live := liveRoutes(p)
	if allow == allowed && len(live) > 0 && t.from[StatusPushed] == needsApproval {
		allow = needsApproval
	}

	switch allow {
	case allowed:
		return nil

	case needsApproval:
		if !override {
			return fmt.Errorf("%w (%s) — %s requires manual approval (override), and will recall the route of any truck it drops",
				ErrPushed, liveRouteNames(live), t.gerund)
		}
		who := approvedBy
		if who == "" {
			who = "an approver"
		}
		p.PushedOverrides = append(p.PushedOverrides, PushedOverride{
			Action:     action,
			ApprovedBy: who,
			ApprovedAt: time.Now(),
			Note: fmt.Sprintf("%s approved with %d route(s) live on the dispatch board (%s)",
				t.gerund, len(live), liveRouteNames(live)),
		})
		return nil

	default: // notListed
		if p.Status == StatusPushed {
			return refusedf("cannot %s: this plan is already on the dispatch board — re-assign, re-pack or re-sequence it first, which takes it back to %s",
				t.verb, StatusPacked)
		}
		return refusedf("cannot %s: this plan is %s — %s", t.verb, statusLabel(p.Status), t.prereq)
	}
}

// walkBackAfterReshuffle invalidates the later-stage artifacts a mid-workflow
// change has just made stale. It is the one place that decides it, so
// resequence, priority and dimension overrides cannot drift apart.
//
// A plan walked back from PUSHED keeps its LiveRoutes: those routes really are
// still on the dealer's board. They are not orphans — the trucks still exist in
// this plan — so they need no recall, only a re-push, which the per-load digest
// forces because the route content has changed.
func walkBackAfterReshuffle(p *Plan) {
	if p.Status == StatusReviewed || p.Status == StatusPushed {
		p.Status = StatusPacked
	}
}
