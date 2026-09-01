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
//
// It returns the names rather than a joined string so a caller can append the
// board's own unnamed trucks (divergenceTrucks) to the SAME list. Two lists
// joined separately is how one of them ends up missing from a sentence.
func liveRouteNames(live []LiveRoute) []string {
	names := make([]string, 0, len(live))
	for _, r := range live {
		if r.VehicleName != "" {
			names = append(names, r.VehicleName)
			continue
		}
		names = append(names, r.VehicleID)
	}
	return names
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
	return gateTransitionOverBoard(p, action, liveRoutes(p), nil, override, approvedBy)
}

// gateTransitionOverBoard is gateTransition told what the DISPATCH BOARD says,
// rather than left to infer it from the plan.
//
// It takes two extra inputs, and both exist because the board and the ledger can
// disagree:
//
//   - live is what to treat as this plan's live routes. gateTransition passes
//     the raw ledger, which is right for every action that reads no board.
//     Assign passes boardTruth.backedLiveRoutes — the claims the board actually
//     backs — so a plan whose ledger is all ghosts is not made to beg an
//     approval for routes that do not exist.
//   - orphans are routes the BOARD holds for this date that no plan of ours
//     names. They escalate this transition on their own, with no ledger
//     involvement at all, because a re-assignment is a writer to that date's
//     board: it decides which trucks stop existing and recalls them by
//     (vehicle_id, scheduled_date). Running it over a board holding runs nobody
//     can account for is the same class of act as re-planning over one, and
//     until this existed it was the identical harm with no gate on it —
//     Ingest refused with a 423 and Assign answered 200 in every one of the
//     four states that produce an orphan.
//
// A CONTESTED orphan never reaches the approval: see refuseContested.
func gateTransitionOverBoard(p *Plan, action string, live []LiveRoute, orphans []BoardDivergence, override bool, approvedBy string) error {
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
	// PUSHED escalate, so the table stays the single source of truth — and an
	// unnamed route on the board escalates through the same door, because the
	// consequence being approved is the same one.
	if allow == allowed && (len(live) > 0 || len(orphans) > 0) && t.from[StatusPushed] == needsApproval {
		allow = needsApproval
	}

	switch allow {
	case allowed:
		return nil

	case needsApproval:
		consequence := "will recall the route of any truck it drops"
		if len(orphans) > 0 {
			// Spelled out rather than folded into the sentence above, because
			// approving this does something DIFFERENT to these routes: nothing.
			// A re-assignment re-shuffles one plan; it has no replacement to put
			// on the board in an unnamed run's place, so withdrawing one would
			// cancel a delivery and leave the slot empty. The approval says "I
			// know these are there and this run may be re-planned around them",
			// and the sentence has to say so or the approver will read it as the
			// re-plan prompt, which DOES withdraw them.
			consequence += fmt.Sprintf(", and re-shuffles this run over %d route(s) the dispatch board holds that NO plan of ours names (%s) — a dispatcher may have created them in GableLBM. This action does NOT withdraw them; approve only if this run may be re-planned around them",
				len(orphans), strings.Join(divergenceTrucks(orphans), ", "))
		}
		return requireApproval([]*Plan{p}, live, orphans, action, t.gerund,
			consequence, override, approvedBy)

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
// # The authority is the BOARD, not the ledger
//
// This gate used to take exactly one input: the ledgers of the plans holding
// the date. That was the deepest defect on the branch and no amount of locking
// could reach it. Plan.LiveRoutes is a CACHE of GableLBM's dispatch board, and
// a crash between the ERP write and the ledger write leaves the two disagreeing
// with nothing able to notice — so a gate reading only the ledger is a gate
// that can be talked out of refusing by a lie, in exactly the direction that
// hurts: an empty ledger over a full board reads as "nothing live here, plan
// away".
//
// It now takes both, and treats them as what they are:
//
//   - prev is still the ledger side. A plan whose ledger claims live routes is
//     something a re-plan strands, and the ledger is where the recall that
//     un-strands it is tombstoned, so it cannot simply be dropped. It has,
//     however, already been RECONCILED against the board by the time it gets
//     here: stale claims were tombstoned (see repairGhostClaims), so this gate
//     no longer demands an approval for routes that do not exist.
//   - orphans is the board side, and it is the half that closes the harm. A
//     live route the board holds that no ledger names was, until now,
//     completely invisible: a re-ingest sailed straight over it and the new
//     plan had no idea the truck was taken. Now the gate SEES it, and a
//     re-plan over it is refused.
//
// # What is deliberately NOT here
//
// Only ORPHAN divergences are passed in. A DISPATCHED route — IN_TRANSIT or
// COMPLETED — must never gate a re-plan, and that is not leniency: GableLBM
// answers every recall of a departed route with a terminal 409, so an approval
// could not clear such a refusal and the date would become permanently
// un-re-plannable by any action a dispatcher can take. A gate whose refusal has
// no remedy is a bug, not a safeguard. The same holds for a route with no
// vehicle, which the (vehicle_id, scheduled_date) recall key cannot even name.
// Both are reported instead — see BoardReportForDate.
//
// # The refusal, and what approving it means
//
// It stays ErrPushed with an approver — the same 423 idiom as the lock and as
// every other live-route gate — but the sentence now distinguishes the two
// sources, because they mean very different things to the person reading it.
// Recalling a route a previous plan of ours pushed is routine. Recalling a
// route that no plan of ours names may be cancelling a delivery a dispatcher
// built by hand and a driver is about to leave on, so approving must be a
// decision made about NAMED trucks, never a side effect of clicking through the
// same prompt as last time.
func gateSupersede(truth boardTruth, prev []*Plan, orphans []BoardDivergence, date string, override bool, approvedBy string) error {
	// A route we cannot withdraw without withdrawing our own is refused before
	// anything is offered for approval. It is FIRST because the alternative is
	// worse than not gating at all: the prompt below would promise to withdraw a
	// run it cannot name, and an approval given against that promise cancels a
	// delivery. See refuseContested.
	if err := refuseContested(orphans, date); err != nil {
		return err
	}

	// Nothing holding this date has anything on the board and the board holds
	// nothing we do not name: the common case, and it must cost nothing.
	if len(prev) == 0 && len(orphans) == 0 {
		return nil
	}

	// Named from the BOARD, not from the ledgers. prev is already filtered to
	// plans the board backs, but a plan can hold one real claim and one ghost,
	// and a prompt that named the ghost would ask for approval to cancel a run
	// that does not exist.
	live := truth.backedLiveRoutes(prev)
	var clauses []string
	if len(prev) > 0 {
		clauses = append(clauses, fmt.Sprintf("will recall every route %s left on the dispatch board", planIDs(prev)))
	}
	if len(orphans) > 0 {
		clauses = append(clauses, fmt.Sprintf(
			"will ALSO recall %d route(s) the dispatch board holds that NO plan of ours names (%s) — a dispatcher may have created them in GableLBM and a truck may be about to drive one, so approve only if you know these are yours to cancel",
			len(orphans), strings.Join(divergenceTrucks(orphans), ", ")))
	}
	return requireApproval(prev, live, orphans, actionSupersede,
		fmt.Sprintf("re-planning %s", date),
		strings.Join(clauses, ", and "),
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
//
// live is the routes to NAME, handed in rather than derived from plans,
// because the two callers disagree about what counts. A transition gate names
// the plan's whole ledger; the supersede gate names only what the dispatch
// board actually backs, since a ghost in an approval prompt asks a dispatcher
// to weigh cancelling a run that is not there.
//
// orphans are trucks the BOARD holds that no plan names. They are named in the
// same sentence as the ledger's own trucks, because a dispatcher weighing an
// approval needs one list of what is about to come off the board, not two. They
// are counted in the recorded note for the same reason, and they belong to no
// plan, so when they are the ONLY reason for the approval there is no plan here
// to record it on — Ingest records that case on the plan it creates, which is
// the only durable artifact of a re-plan that superseded nothing.
func requireApproval(plans []*Plan, live []LiveRoute, orphans []BoardDivergence, action, gerund, consequence string, override bool, approvedBy string) error {
	names := append(liveRouteNames(live), divergenceTrucks(orphans)...)
	n := len(live) + len(orphans)
	if !override {
		return fmt.Errorf("%w (%s) — %s requires manual approval (override), and %s",
			ErrPushed, strings.Join(names, ", "), gerund, consequence)
	}
	who := approverOrDefault(approvedBy)
	for _, p := range plans {
		p.PushedOverrides = append(p.PushedOverrides, PushedOverride{
			Action:     action,
			ApprovedBy: who,
			ApprovedAt: time.Now(),
			Note: fmt.Sprintf("%s approved with %d route(s) live on the dispatch board (%s)",
				gerund, n, strings.Join(names, ", ")),
		})
	}
	return nil
}

// approverOrDefault names who authorized something. It is deliberately never an
// empty string: "who approved cancelling this truck's route?" must not be
// answerable with a blank.
func approverOrDefault(approvedBy string) string {
	if approvedBy == "" {
		return "an approver"
	}
	return approvedBy
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
