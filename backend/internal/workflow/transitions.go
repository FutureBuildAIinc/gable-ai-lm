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
// Re-ingest is deliberately absent. It does not move a plan through this table;
// it mints a NEW plan for a date and leaves the old one behind, so no cell of
// any row describes it. It is gated by gateSupersede, which asks the one
// question the table cannot: does the plan this re-ingest replaces still have
// routes on the dealer's board?
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
	// actionSupersede is not a transition OF a plan and so has no row in the
	// table below: it is a plan being REPLACED by a fresh ingest of its date.
	// See gateSupersede for why it keys on the ledger alone.
	actionSupersede = "supersede"
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

// liveRoutesAcross is liveRoutes over a set of plans, in plan order. It is what
// makes one refusal sentence able to describe a whole date: a date can hold
// several plans and any number of them may be holding trucks on the board.
func liveRoutesAcross(plans []*Plan) []LiveRoute {
	out := []LiveRoute{}
	for _, p := range plans {
		out = append(out, liveRoutes(p)...)
	}
	return out
}

// planIDs names the plans a refusal is about, so a dispatcher reading it can go
// and look at them. "Some other plan has routes out" is not actionable.
func planIDs(plans []*Plan) string {
	ids := make([]string, 0, len(plans))
	for _, p := range plans {
		ids = append(ids, p.ID)
	}
	return strings.Join(ids, ", ")
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
		return requireApproval([]*Plan{p}, action, t.gerund,
			"will recall the route of any truck it drops", override, approvedBy)

	default: // notListed
		if p.Status == StatusPushed {
			return refusedf("cannot %s: this plan is already on the dispatch board — re-assign, re-pack or re-sequence it first, which takes it back to %s",
				t.verb, StatusPacked)
		}
		return refusedf("cannot %s: this plan is %s — %s", t.verb, statusLabel(p.Status), t.prereq)
	}
}

// gateSupersede is the ingest-side gate, and it exists because gateTransition
// structurally cannot see the harm it closes.
//
// Ingest does not mutate a plan — it mints a new one for a date and leaves the
// old one behind. Every gate above keys on p.Status and p.LiveRoutes of the
// plan being changed, and the new plan has neither: it is born ANALYZED with an
// empty ledger. So a dispatcher who says "the day changed, re-run it" on a date
// whose routes are already live used to get a brand-new plan whose recall
// machinery was scoped to its own empty ledger and could therefore NEVER
// withdraw what the superseded plan had left on the dealer's board. That is the
// same orphan the state machine was written to remove — "the yard loads a truck
// for a run that no longer exists" — reached by what is plausibly the more
// common dispatcher action of the two.
//
// So the gate keys on the ledgers of the plans being SUPERSEDED, and on nothing
// else. Not on their status: a plan that reached PUSHED and has since had every
// route recalled has nothing on the board, and re-planning that date must stay
// exactly as frictionless as it is today. "Are there live routes?" is the whole
// question.
//
// And it keys on EVERY plan holding the date, not the newest. A date holds one
// plan per ingest — that is what "supersede rather than replace" means — and
// the live one need not be the latest: pushing an older plan by id after a
// re-ingest has minted a successor leaves the newest plan's ledger empty and
// the board full. A latest-only gate walks straight past that, which three
// plain HTTP calls demonstrated. prev is the union (see Service.supersededPlans)
// and is already filtered to plans with something live, so an empty slice is
// the frictionless path and costs nothing.
//
// The refusal is ErrPushed with an approver — the same 423 idiom as the lock
// and as every other live-route gate — and an exercised override is recorded on
// every superseded plan, where the recalls it authorizes will be tombstoned.
func gateSupersede(prev []*Plan, date string, override bool, approvedBy string) error {
	// Nothing holding this date has anything on the board: the common case, and
	// it must cost nothing.
	if len(prev) == 0 {
		return nil
	}
	return requireApproval(prev, actionSupersede,
		fmt.Sprintf("re-planning %s", date),
		fmt.Sprintf("will recall every route %s left on the dispatch board", planIDs(prev)),
		override, approvedBy)
}

// requireApproval writes the live-route refusal, and — when the override is
// exercised — records the approval on the plan.
//
// It is one function rather than one per gate so that every 423 in this package
// reads the same way and every approval lands in the same audit field. The
// callers supply only what differs: the gerund naming the operation, and the
// consequence clause promising what an approval will do to the dealer's board.
//
// It takes a SET of plans because a transition gate is about one plan and the
// supersede gate is about a whole date, which may be held by several. One
// sentence still comes out — the live trucks named across all of them — and the
// approval is recorded on each plan it authorizes changing, because that is
// where the recall it pays for will be tombstoned. A second sentence for the
// multi-plan case would be a second override idiom for a dispatcher to learn.
func requireApproval(plans []*Plan, action, gerund, consequence string, override bool, approvedBy string) error {
	live := liveRoutesAcross(plans)
	if !override {
		return fmt.Errorf("%w (%s) — %s requires manual approval (override), and %s",
			ErrPushed, liveRouteNames(live), gerund, consequence)
	}
	who := approvedBy
	if who == "" {
		who = "an approver"
	}
	for _, p := range plans {
		p.PushedOverrides = append(p.PushedOverrides, PushedOverride{
			Action:     action,
			ApprovedBy: who,
			ApprovedAt: time.Now(),
			Note: fmt.Sprintf("%s approved with %d route(s) live on the dispatch board (%s)",
				gerund, len(live), liveRouteNames(live)),
		})
	}
	return nil
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
