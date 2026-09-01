// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

// The dispatch board is the authority. This file is where that stops being a
// slogan.
//
// Everything else in this package gates on Plan.LiveRoutes — the LEDGER, which
// is this service's record of what it believes it put on GableLBM's dispatch
// board. Nine rounds of work went into making that ledger correct under
// concurrency and it now is: one writer per dispatch date, ten writer pairs,
// zero acceptance violations. None of it closes the window that matters most,
// because no lock can: a crash between the ERP write and the ledger write
// leaves the two disagreeing, and nothing in this service could ever discover
// that. Every gate keyed on the ledger was therefore reasoning from something
// that might be a lie.
//
// GableLBM is the system of record. ReplaceDeliveryRoute DELETEs every
// DRAFT/SCHEDULED delivery_route for a (vehicle_id, scheduled_date) before it
// inserts, so the board — and nothing else — knows the truth about who owns a
// truck on a day. Now that the seam can READ it
// (gable.Client.ListDeliveryRoutesForDate), the ledger is demoted to what it
// always was: a cache, useful, and never the thing a decision rests on.
//
// Two kinds of disagreement fall out, and they are NOT symmetric. That
// asymmetry is the whole design:
//
//   - A GHOST is our record being wrong about our own state: a ledger entry
//     claiming a truck whose route the board does not hold. Repairing it takes
//     nothing off anybody's board — there is nothing there — so it is safe,
//     automatic, and idempotent. Left alone it makes a re-plan demand an
//     approval for routes that do not exist, and the recall it authorizes is
//     keyed (vehicle, date), so it would cancel whatever route the truck
//     acquires next.
//
//   - An ORPHAN is the board holding a live route that NO plan's ledger names.
//     This is the harm the whole branch exists to prevent, and it is exactly
//     the one that must NOT be auto-repaired: a dispatcher may have created
//     that route by hand, and a truck may be about to drive it. Withdrawing it
//     unasked would cancel a real delivery. So an orphan is SURFACED — the
//     re-plan gate sees it and refuses, and it appears in a report a human can
//     act on — and it is withdrawn only under the same explicit approval the
//     rest of this package uses for board-changing acts.
//
// Two more things the board can say, neither of which is reclaimable and
// neither of which may ever gate a re-plan:
//
//   - DISPATCHED: IN_TRANSIT or COMPLETED. The truck has left the yard. A gate
//     that refused a re-plan over one would be un-actionable — no override can
//     help, because GableLBM answers every recall of a dispatched route with a
//     terminal 409 — so the date would become permanently un-re-plannable. It
//     is reported and never touched.
//   - UNADDRESSABLE: a live route with a NULL vehicle_id upstream. The recall
//     key is (vehicle_id, scheduled_date), so there is no way to name this
//     route on the wire at all. Same reasoning: report it, never gate on it.
//
// # Matching is by ROUTE ID, and that is the third thing this file is about
//
// A claim is paired with the board row it NAMES, not with any row that happens
// to share its truck. The ERP hands back delivery_routes.id on every write-back
// (gable.RouteAck) and LiveRoute now carries it. Before that, "does the board
// back this claim?" was asked as "does the board hold anything for this truck
// on this day?" — and the ERP does not make (vehicle_id, scheduled_date)
// unique. Migration 009 declines the index; internal/delivery/repository.go's
// CreateRoute, the dealer's own dispatch UI, inserts with no dedup. So a
// dispatcher's hand-built second run for a truck we already had a route on
// answered "yes" to every question this file asks: it stood in for our row, the
// report read in sync with zero divergences, and an approved re-plan recalled
// the truck and destroyed a run it had never named.
//
// Two consequences follow, and both are load-bearing:
//
//   - The board view keeps BOTH rows. Collapsing to one per truck is what let
//     the second one hide.
//   - An unmatched row is an ORPHAN even when we hold a claim on that truck —
//     but a special one. The recall wire call is keyed (vehicle_id,
//     scheduled_date) and cannot name a single row, so withdrawing that orphan
//     would withdraw ours too. There is no approval that makes that precise, so
//     it is marked CONTESTED and REFUSED (422) rather than offered for
//     approval. The remedy is in GableLBM, and the report names the row.
//
// # An unrecognised status counts as HELD
//
// delivery_routes.status is VARCHAR(50) with no CHECK constraint. A value this
// service does not know — ON_HOLD, say, which real Postgres accepts and the
// endpoint returns verbatim — used to answer false to both Live() and
// Dispatched(), so the row read as "no such route": the claim on it was
// tombstoned automatically, with no wire call and no way back, and the row
// itself fell out of the orphan pass too and was reported nowhere. A status we
// cannot classify means the board holds something we do not understand. It
// counts as held, it is always reported (UNKNOWN_STATUS), and it makes InSync
// false — because the one thing it must never do is authorize destroying
// something.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// ErrBoardUnreadable reports that GableLBM's dispatch board could not be read,
// which this service treats as a refusal rather than as permission to fall back
// on its own ledger.
//
// It is a distinct sentinel because it must NOT be mapped to the 423 approval
// idiom. An approval means "yes, I accept the consequence you just described";
// here there is no described consequence, because the consequence is unknown —
// that is what unreadable means. An override on this path would mean "plan over
// a board you cannot see", which is precisely the harm class.
var ErrBoardUnreadable = errors.New("could not read GableLBM's dispatch board")

// Divergence kinds. See the file header for why the four are not interchangeable.
const (
	// DivergenceGhost — a ledger entry the board does not back. Auto-repaired.
	DivergenceGhost = "GHOST"
	// DivergenceOrphan — a live board route no ledger names. Surfaced, never
	// silently recalled.
	DivergenceOrphan = "ORPHAN"
	// DivergenceDispatched — a board route whose truck has left the yard.
	// Reported; never reclaimable, so never a gate.
	DivergenceDispatched = "DISPATCHED"
	// DivergenceUnaddressable — a live board route with no vehicle, which the
	// (vehicle_id, scheduled_date) recall key cannot name. Reported only.
	DivergenceUnaddressable = "UNADDRESSABLE"
	// DivergenceUnknownStatus — a board route whose delivery_routes.status this
	// service does not recognise. Always reported, always counted as held,
	// never tombstoned and never repaired: we do not know what it is, so the
	// only safe act is to show a human.
	DivergenceUnknownStatus = "UNKNOWN_STATUS"
)

// divergenceRank orders a report the way a human reads it: what we repaired,
// what needs a decision, then what is merely context. It also makes every
// refusal sentence and every JSON body deterministic, which is what lets them
// be asserted at all.
var divergenceRank = map[string]int{
	DivergenceGhost:         0,
	DivergenceOrphan:        1,
	DivergenceUnknownStatus: 2,
	DivergenceDispatched:    3,
	DivergenceUnaddressable: 4,
}

// BoardDivergence is one disagreement between GableLBM's dispatch board and
// this service's ledger for a date.
//
// It carries the board's own row (route id, status, stop count, order ids) and
// not just a vehicle id, because the human who has to act on an orphan needs to
// see what is ON the truck before deciding whether cancelling it is safe.
// StopCount alone cannot answer that: a re-plan that swaps one order for
// another leaves the count identical.
type BoardDivergence struct {
	Kind        string   `json:"kind"`
	VehicleID   string   `json:"vehicle_id,omitempty"`
	VehicleName string   `json:"vehicle_name,omitempty"`
	RouteID     string   `json:"route_id,omitempty"`
	BoardStatus string   `json:"board_status,omitempty"`
	StopCount   int      `json:"stop_count"`
	OrderIDs    []string `json:"order_ids,omitempty"`
	// PlanID names the plan whose ledger is wrong. Set on ghosts only — an
	// orphan is defined by no plan naming it.
	PlanID string `json:"plan_id,omitempty"`
	// Contested marks an ORPHAN sharing a truck with a claim the board DOES
	// back. It is still an orphan — no plan of ours names this row — but it is
	// the one kind no approval can authorize withdrawing: RouteRecall is keyed
	// (vehicle_id, scheduled_date) and would take our row down with it, and
	// there is no call that names one of the two. So this refuses a re-plan
	// (422) instead of offering the 423 approval prompt, and the fix is to
	// cancel whichever run is wrong in GableLBM.
	Contested bool   `json:"contested,omitempty"`
	Note      string `json:"note"`
}

// BoardReport is the read-only reconciliation of a date: what the board holds,
// which plans claim what, and every way the two disagree.
//
// It exists because "surface it, do not silently recall it" needs a surface. An
// orphan that only ever appears as a 423 on a re-plan somebody happened to
// attempt is not actionable; this is the thing a dispatcher, or support, can
// open and read.
type BoardReport struct {
	Date string `json:"date"`
	// Board is every route on the board for the date, unfiltered — including
	// CANCELLED rows, which is how "did my recall land?" is answered, and
	// including rows whose status this service cannot classify, which is how a
	// human sees the thing UNKNOWN_STATUS is telling them about.
	Board []gable.BoardRoute `json:"board"`
	// Claims maps each truck some plan's ledger believes is live to the plans
	// claiming it. More than one plan on a truck is itself a defect (the ERP
	// cannot represent it) and shows up here rather than having to be inferred.
	Claims      map[string][]string `json:"claims"`
	Divergences []BoardDivergence   `json:"divergences"`
	// InSync is len(Divergences) == 0, stated rather than derived so a caller
	// polling this cannot get the polarity wrong.
	InSync bool `json:"in_sync"`
}

// boardView is one read of the board, kept as the board actually holds it.
//
// onBoard is a SLICE and not a map keyed by vehicle, and that is the fix for
// the worst defect this file has carried. The old view did
// live[r.VehicleID] = r, so a second live row for one truck silently
// OVERWROTE the first — and the ERP genuinely produces that state, because
// migration 009 declines a unique index on (vehicle_id, scheduled_date) and the
// dealer's own CreateRoute inserts with no dedup. The row that got overwritten
// was the one nobody could see.
//
// CANCELLED rows are dropped here and only here: they are not on the board, and
// nothing may match, gate or report against them. They stay in routes, which is
// what the report renders, because a CANCELLED row with RecalledAt is how "did
// my recall land?" is answered.
type boardView struct {
	date   string
	routes []gable.BoardRoute
	// onBoard is every row the board holds that is not CANCELLED, in the order
	// the ERP returned it: live, departed, vehicle-less, and the ones whose
	// status this service does not recognise. Order is the ERP's, which makes
	// legacy claim matching (below) deterministic.
	onBoard []gable.BoardRoute
	// byRouteID indexes onBoard by GableLBM's own delivery_routes.id. First
	// occurrence wins: a duplicate id is an upstream impossibility (it is the
	// primary key), and silently re-pointing the index would make matching
	// depend on scan order.
	byRouteID map[string]int
}

// newBoardView indexes a board read.
func newBoardView(date string, routes []gable.BoardRoute) boardView {
	v := boardView{date: date, routes: routes, byRouteID: map[string]int{}}
	for _, r := range routes {
		if r.Status == gable.RouteStatusCancelled {
			continue
		}
		i := len(v.onBoard)
		v.onBoard = append(v.onBoard, r)
		if r.RouteID == "" {
			continue
		}
		if _, dup := v.byRouteID[r.RouteID]; !dup {
			v.byRouteID[r.RouteID] = i
		}
	}
	return v
}

// claimKey identifies ONE ledger claim: the plan that holds it and the truck it
// names.
//
// A plan holds at most one LIVE claim per truck — markRouteLive revives an
// existing entry rather than appending a second — so this pair names exactly one
// LiveRoute, which is what lets a match be recorded without carrying the whole
// entry around.
type claimKey struct{ planID, vehicleID string }

// boardTruth is ONE board read reconciled against ONE set of ledgers: which
// board row backs each claim, and every way the two disagree.
//
// It exists because "does the board back this claim?" and "does anything on the
// board belong to nobody?" are two halves of the SAME pairing and were
// previously computed by two independent loops over a vehicle-keyed map. That is
// how a row could be both invisible to the orphan pass (its truck was claimed)
// and unable to back the claim that hid it. Pair once; read the result twice.
//
// It is a pure value: nothing here writes, recalls or gates.
type boardTruth struct {
	view  boardView
	plans []*Plan
	// backing maps each claim to its row's index in view.onBoard. A claim
	// absent from this map is a GHOST.
	backing map[claimKey]int
	divs    []BoardDivergence
}

// reconcile pairs a date's ledgers with the board and names every disagreement.
//
// The pairing runs in two passes, and the order between them is the whole
// contract for a ledger written before route ids existed:
//
//  1. Claims that CARRY a route id are matched to the row with that id, exactly.
//     A claim whose id is not on the board is a ghost even if its truck holds
//     some other row — that row is somebody else's, which is precisely the case
//     this change exists to see.
//  2. Claims with NO route id — every claim written before this change, and any
//     whose push got no readable acknowledgement — are matched by TRUCK, to a
//     row no id-bearing claim already took. That is the old behaviour, kept
//     deliberately and kept SECOND: an id-less claim can therefore never be
//     invented into a ghost (a row for its truck backs it, as it always did) and
//     can never leave its own row looking like an orphan. What it does lose is
//     the ability to hide a SECOND row for the same truck — it consumes one row,
//     not the truck — so the defect closes for old ledgers too.
//
// Whatever is left over on each side is a divergence.
func (v boardView) reconcile(plans []*Plan) boardTruth {
	t := boardTruth{view: v, plans: plans, backing: map[claimKey]int{}}
	taken := make([]bool, len(v.onBoard))

	for _, p := range plans {
		for _, r := range liveRoutes(p) {
			if r.RouteID == "" {
				continue
			}
			i, ok := v.byRouteID[r.RouteID]
			if !ok || taken[i] {
				continue
			}
			taken[i] = true
			t.backing[claimKey{p.ID, r.VehicleID}] = i
		}
	}
	for _, p := range plans {
		for _, r := range liveRoutes(p) {
			if r.RouteID != "" {
				continue
			}
			for i, br := range v.onBoard {
				if taken[i] || br.VehicleID != r.VehicleID {
					continue
				}
				taken[i] = true
				t.backing[claimKey{p.ID, r.VehicleID}] = i
				break
			}
		}
	}

	// GHOSTS: our record is wrong about our own state.
	for _, p := range plans {
		for _, r := range liveRoutes(p) {
			if _, ok := t.backing[claimKey{p.ID, r.VehicleID}]; ok {
				continue
			}
			t.divs = append(t.divs, BoardDivergence{
				Kind:        DivergenceGhost,
				VehicleID:   r.VehicleID,
				VehicleName: r.VehicleName,
				RouteID:     r.RouteID,
				PlanID:      p.ID,
				Note: fmt.Sprintf("plan %s claims truck %s is live on %s, but the dispatch board holds no such route — the claim is stale and is being tombstoned",
					p.ID, nameOrID(r), v.date),
			})
		}
	}

	// Which trucks do we hold a BACKED claim on? Not "claim" — backed. An
	// orphan on a truck whose only claim is a ghost is an ordinary orphan: a
	// recall keyed (vehicle, date) withdraws exactly it and nothing of ours.
	backedTruck := map[string]bool{}
	for k := range t.backing {
		backedTruck[k.vehicleID] = true
	}

	// The board side. One switch, one kind per row, in the order a row must be
	// judged: what we cannot classify, then what we cannot address, then what
	// we already accounted for, then what is left.
	for i, r := range v.onBoard {
		switch {
		case r.Unknown():
			// Reported whether or not a claim matched it. A matched row still
			// counts as held — the claim is not a ghost and is never
			// tombstoned — but the board is holding something this service
			// does not understand, and that is a fact a human has to see.
			t.divs = append(t.divs, BoardDivergence{
				Kind:        DivergenceUnknownStatus,
				VehicleID:   r.VehicleID,
				RouteID:     r.RouteID,
				BoardStatus: r.Status,
				StopCount:   r.StopCount,
				OrderIDs:    r.OrderIDs,
				Note: fmt.Sprintf("the dispatch board holds a route on %s for truck %s in status %q, which this service does not recognise — delivery_routes.status has no CHECK constraint upstream, so this cannot be classified as live, departed or cancelled; it is counted as HELD and nothing about it is repaired or withdrawn automatically",
					v.date, nameOrEmpty(r.VehicleID), r.Status),
			})
		case r.VehicleID == "":
			t.divs = append(t.divs, BoardDivergence{
				Kind:        DivergenceUnaddressable,
				RouteID:     r.RouteID,
				BoardStatus: r.Status,
				StopCount:   r.StopCount,
				OrderIDs:    r.OrderIDs,
				Note: fmt.Sprintf("the dispatch board holds a %s route on %s with no vehicle assigned — the recall key is (vehicle_id, scheduled_date), so nothing this service can send names it; it has to be resolved in GableLBM",
					r.Status, v.date),
			})
		case taken[i]:
			// A plan of ours names this exact row. Nothing to report.
		case r.Live():
			d := BoardDivergence{
				Kind:        DivergenceOrphan,
				VehicleID:   r.VehicleID,
				RouteID:     r.RouteID,
				BoardStatus: r.Status,
				StopCount:   r.StopCount,
				OrderIDs:    r.OrderIDs,
				Contested:   backedTruck[r.VehicleID],
				Note: fmt.Sprintf("the dispatch board holds a %s route for truck %s on %s with %d stop(s) that no plan of ours names — it may have been created in GableLBM by a dispatcher, so it is NOT withdrawn without an approval",
					r.Status, r.VehicleID, v.date, r.StopCount),
			}
			if d.Contested {
				d.Note = fmt.Sprintf("the dispatch board holds a SECOND %s route for truck %s on %s (route %s, %d stop(s)) that no plan of ours names, alongside one that we do — it was almost certainly built in GableLBM by hand. The recall key is (vehicle_id, scheduled_date), so nothing this service can send withdraws one of the two without the other; cancel whichever run is wrong in GableLBM",
					r.Status, r.VehicleID, v.date, r.RouteID, r.StopCount)
			}
			t.divs = append(t.divs, d)
		case r.Dispatched():
			t.divs = append(t.divs, BoardDivergence{
				Kind:        DivergenceDispatched,
				VehicleID:   r.VehicleID,
				RouteID:     r.RouteID,
				BoardStatus: r.Status,
				StopCount:   r.StopCount,
				OrderIDs:    r.OrderIDs,
				Note: fmt.Sprintf("truck %s is %s on %s — it has left the yard, so its route can never be recalled and must not be treated as reclaimable",
					r.VehicleID, r.Status, v.date),
			})
		}
	}

	sortDivergences(t.divs)
	return t
}

// divergences is the whole report, never nil, so a caller can range it and a
// JSON body never carries null where an array belongs.
func (t boardTruth) divergences() []BoardDivergence {
	if t.divs == nil {
		return []BoardDivergence{}
	}
	return t.divs
}

// ofKind filters this reconciliation. It is the one helper rather than three
// bespoke loops so "which kind is this code acting on?" is always visible at the
// call site — the distinction the whole file is about.
func (t boardTruth) ofKind(kind string) []BoardDivergence {
	return divergencesOfKind(t.divs, kind)
}

// ghosts and orphans name the two halves callers act on differently: a ghost is
// repaired without asking, an orphan is never touched without an approval.
func (t boardTruth) ghosts() []BoardDivergence  { return t.ofKind(DivergenceGhost) }
func (t boardTruth) orphans() []BoardDivergence { return t.ofKind(DivergenceOrphan) }

// backs reports whether the board holds the row this claim names.
func (t boardTruth) backs(planID, vehicleID string) bool {
	_, ok := t.backing[claimKey{planID, vehicleID}]
	return ok
}

// backedLivePlans is "which plans does a re-plan actually strand?", answered
// against the BOARD rather than against the ledgers alone.
//
// A plan counts only if it holds a claim the board really backs. A plan whose
// every claim is a ghost strands NOTHING — there is nothing on the dealer's
// board to leave behind — so gating a re-plan on it would demand an approval for
// routes that do not exist, and the recall that approval authorizes is keyed
// (vehicle, date), which means it would cancel whatever route those trucks
// acquire next.
func (t boardTruth) backedLivePlans() []*Plan {
	out := make([]*Plan, 0, len(t.plans))
	for _, p := range t.plans {
		for _, r := range liveRoutes(p) {
			if t.backs(p.ID, r.VehicleID) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// backedLiveRoutes is the ledger entries of these plans that the board actually
// backs — the trucks a re-plan or a re-assignment would really be taking off the
// dealer's board.
//
// It exists so a refusal names only routes that exist, and so a RECALL SET is
// computed from what the board holds rather than from what we believe. Naming a
// ghost in an approval prompt is not a cosmetic slip: it asks a dispatcher to
// weigh cancelling a run that is not there, and the recall they would be
// approving is keyed (vehicle, date), so exercising it would cancel whatever
// route that truck acquires next.
func (t boardTruth) backedLiveRoutes(plans []*Plan) []LiveRoute {
	out := []LiveRoute{}
	for _, p := range plans {
		for _, r := range liveRoutes(p) {
			if t.backs(p.ID, r.VehicleID) {
				out = append(out, r)
			}
		}
	}
	return out
}

// contestedOrphans are the orphans no approval can authorize withdrawing. See
// BoardDivergence.Contested.
func contestedOrphans(orphans []BoardDivergence) []BoardDivergence {
	out := make([]BoardDivergence, 0, len(orphans))
	for _, d := range orphans {
		if d.Contested {
			out = append(out, d)
		}
	}
	return out
}

// refuseContested is the gate for a board this service cannot act on precisely.
//
// It is a Refusal (422) and NOT the 423 approval idiom, and the difference is
// the whole point. A 423 says "approve and I will do the thing I just
// described"; here the described thing is impossible. RouteRecall is keyed
// (vehicle_id, scheduled_date), the board holds two rows for that pair, and
// withdrawing the one nobody named would withdraw ours as well. Offering an
// approval would be offering a promise this service cannot keep — and it is
// exactly the promise that, unkept, cancels a run a driver is about to leave on.
//
// The refusal HAS a remedy, which is what separates it from the un-actionable
// gates this file refuses to build: a human cancels whichever run is wrong in
// GableLBM and the date re-plans normally. BoardReportForDate names the row, its
// status, its stop count and its orders so they can tell which one that is.
func refuseContested(orphans []BoardDivergence, date string) error {
	contested := contestedOrphans(orphans)
	if len(contested) == 0 {
		return nil
	}
	return refusedf("the dispatch board holds %d route(s) for %s that no plan of ours names, on truck(s) we also hold a route on (%s) — almost certainly built in GableLBM by hand. A recall names (vehicle_id, scheduled_date), so withdrawing one of the two would withdraw ours as well and no approval can make that precise: cancel whichever run is wrong in GableLBM, then re-plan. See GET /api/v1/workflow/dispatch-board?date=%s for what is on each",
		len(contested), date, strings.Join(divergenceTrucks(contested), ", "), date)
}

// nameOrEmpty renders a vehicle id for a sentence, so a route with none reads as
// something rather than as a hole in the middle of a message.
func nameOrEmpty(vehicleID string) string {
	if vehicleID == "" {
		return "(none)"
	}
	return vehicleID
}

// readBoard reads the date's dispatch board, or refuses.
//
// FAIL CLOSED. There is no fallback to the ledger here and there must never be
// one: the ledger is the thing this read exists to stop trusting, and the
// moment an unreachable ERP silently demoted the board back to "advisory", the
// entire harm class would return on a network blip — on precisely the
// deployments where a flaky ERP has most likely just left a half-written push
// behind. Refusing costs a dispatcher a retry. Failing open costs a truck
// loading for a run that no longer exists.
func (s *Service) readBoard(ctx context.Context, date string) (boardView, error) {
	routes, err := s.gable.ListDeliveryRoutesForDate(ctx, date)
	if err != nil {
		return boardView{}, fmt.Errorf(
			"%w for %s, so the day was not re-planned — this service will not plan over a board it cannot see, and no approval can authorize that; check GableLBM and try again: %w",
			ErrBoardUnreadable, date, err)
	}
	return newBoardView(date, routes), nil
}

// claimsOn maps each truck the given plans' ledgers still believe is live to
// the plans claiming it.
func claimsOn(plans []*Plan) map[string][]string {
	out := map[string][]string{}
	for _, p := range plans {
		for _, r := range liveRoutes(p) {
			out[r.VehicleID] = append(out[r.VehicleID], p.ID)
		}
	}
	return out
}

// sortDivergences puts a report in a fixed, readable order so that every
// sentence built from it, and every JSON body, is deterministic.
func sortDivergences(in []BoardDivergence) {
	sort.SliceStable(in, func(i, j int) bool {
		if a, b := divergenceRank[in[i].Kind], divergenceRank[in[j].Kind]; a != b {
			return a < b
		}
		if in[i].VehicleID != in[j].VehicleID {
			return in[i].VehicleID < in[j].VehicleID
		}
		// Route id, before plan id. A truck can now legitimately carry more
		// than one row (that is the defect this file was rewritten for), so
		// vehicle+kind stopped being unique and a tie broken only on plan id
		// left two orphans on one truck in an order that depended on the ERP's
		// scan. Every sentence and JSON body built from this must be fixed.
		if in[i].RouteID != in[j].RouteID {
			return in[i].RouteID < in[j].RouteID
		}
		return in[i].PlanID < in[j].PlanID
	})
}

// divergencesOfKind filters a report. It is one helper rather than three
// bespoke loops so "which kind is this code acting on?" is always visible at
// the call site — the distinction the whole file is about.
func divergencesOfKind(in []BoardDivergence, kind string) []BoardDivergence {
	out := make([]BoardDivergence, 0, len(in))
	for _, d := range in {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

// divergenceTrucks names the trucks a set of divergences is about, for a
// refusal sentence.
func divergenceTrucks(in []BoardDivergence) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		if d.VehicleName != "" {
			out = append(out, d.VehicleName)
			continue
		}
		out = append(out, d.VehicleID)
	}
	return out
}

// BoardReportForDate reconciles a date against GableLBM's dispatch board and
// reports every divergence, WITHOUT changing anything.
//
// This is the surface an orphan needs. The re-plan gate makes an orphan
// impossible to plan over, which is what actually closes the harm, but a
// refusal only reaches somebody who happened to attempt a re-plan; a route on
// the dealer's board that nobody owns has to be discoverable on its own.
//
// It repairs nothing, deliberately, including the ghosts it reports. A GET that
// writes is a GET that cannot be polled, cannot be given to a dashboard, and
// changes state for a caller who only asked a question. Ghost repair belongs to
// the re-plan path, where it is already running under the dispatch-date claim
// that makes it safe.
func (s *Service) BoardReportForDate(ctx context.Context, date string) (*BoardReport, error) {
	if date == "" {
		return nil, fmt.Errorf("%w: date is required", ErrInvalidRequest)
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, fmt.Errorf("%w: invalid date %q; expected YYYY-MM-DD", ErrInvalidRequest, date)
	}
	plans, err := s.repo.ListForDate(ctx, date)
	if err != nil {
		return nil, fmt.Errorf("look up the existing plans for %s: %w", date, err)
	}
	view, err := s.readBoard(ctx, date)
	if err != nil {
		return nil, err
	}
	live := livePlans(plans)
	divs := view.reconcile(live).divergences()
	board := view.routes
	if board == nil {
		// A bare JSON array, never null: "the board is empty" and "the read
		// failed" must not decode to the same value on the far side.
		board = []gable.BoardRoute{}
	}
	return &BoardReport{
		Date:        date,
		Board:       board,
		Claims:      claimsOn(live),
		Divergences: divs,
		InSync:      len(divs) == 0,
	}, nil
}

// repairGhostClaims tombstones every ledger entry the board does not back, on
// the plan that holds it, and mirrors the correction into the in-memory copies
// the caller is about to gate on.
//
// It is automatic and needs no approval because it takes nothing off anybody's
// board: by definition the board has no such route. What it removes is a claim,
// and a false claim is not a safety margin — it is a gate demanding an approval
// for a recall that would be keyed (vehicle, date) and would therefore cancel
// whatever route that truck acquires NEXT.
//
// Idempotent by construction: a second run reconciles against ledgers that have
// already been tombstoned, finds no ghosts, and writes nothing at all — not a
// no-op UPDATE, not a version bump.
//
// A failure to record a repair is returned rather than swallowed. Proceeding
// would re-plan the date on a ledger this code has already decided is wrong,
// which is the reasoning-from-a-lie the whole change exists to end.
func (s *Service) repairGhostClaims(ctx context.Context, plans []*Plan, ghosts []BoardDivergence, at time.Time) error {
	if len(ghosts) == 0 {
		return nil
	}
	byPlan := map[string]map[string]bool{}
	for _, g := range ghosts {
		if byPlan[g.PlanID] == nil {
			byPlan[g.PlanID] = map[string]bool{}
		}
		byPlan[g.PlanID][g.VehicleID] = true
	}
	for _, p := range plans {
		vehicles := byPlan[p.ID]
		if len(vehicles) == 0 {
			continue
		}
		note := fmt.Sprintf("tombstoned: GableLBM's dispatch board holds no such route for %s, and the board is the system of record", p.PlanDate)
		fresh, n, err := s.tombstoneVehicles(ctx, p.ID, vehicles, at, systemRecaller, note)
		if err != nil {
			return fmt.Errorf("repair the stale claim(s) on plan %s: %w", p.ID, err)
		}
		// ADOPT the persisted copy, so the gate that runs next reads the
		// REPAIRED ledger and the write that may follow carries the CURRENT
		// version. Re-listing instead would ask the store a question the repair
		// has already answered; mirroring the tombstone by hand onto the stale
		// copy would leave its Version behind and turn the repair into a 409
		// the first time supersede() writes that same plan.
		if fresh != nil {
			*p = *fresh
		}
		slog.Warn("repaired a stale live-route claim: the ledger named a route the dispatch board does not hold",
			"plan", p.ID, "date", p.PlanDate, "routes", n, "trucks", keysOf(vehicles))
	}
	return nil
}

// recallOrphans withdraws board routes that no plan names — and runs ONLY under
// an approval that named them.
//
// Every caller reaches this through gateSupersede, which refuses without an
// override and, when one is given, has already spelled out in the sentence the
// approver read exactly which unnamed trucks approving will withdraw. That is
// the difference between this and an auto-repair: a dispatcher may have built
// that route by hand in GableLBM and a truck may be about to drive it, so a
// human agrees to the cancellation by name or it does not happen.
//
// A dispatched truck can never arrive here — gateSupersede is fed only ORPHAN
// divergences, and a departed route is DISPATCHED — but GableLBM's terminal 409
// is still handled, because "this cannot happen" is how the last four defects
// on this branch were introduced. It aborts the whole ingest: a half-cleared
// board with a new plan on top of it is worse than a refusal.
func (s *Service) recallOrphans(ctx context.Context, next *Plan, orphans []BoardDivergence, approvedBy string) error {
	if len(orphans) == 0 {
		return nil
	}
	who := approverOrDefault(approvedBy)
	now := time.Now()
	for _, o := range orphans {
		note := fmt.Sprintf("withdrawn by an approved re-plan of %s: this route was on the dispatch board and no plan named it", next.PlanDate)
		res, err := s.gable.RecallDeliveryRoute(ctx, gable.RouteRecall{
			VehicleID:     o.VehicleID,
			ScheduledDate: next.PlanDate,
			Reason:        note,
			RecalledBy:    who,
		})
		if err != nil {
			slog.Error("could not withdraw an unnamed route from the dispatch board",
				"date", next.PlanDate, "vehicle", o.VehicleID, "route", o.RouteID, "error", err)
			if errors.Is(err, gable.ErrRouteDispatched) {
				return refusedf("truck %s holds a route on %s that no plan of ours names and it has already left the yard, so it cannot be recalled and this date cannot be re-planned around it; call the driver first",
					o.VehicleID, next.PlanDate)
			}
			return fmt.Errorf("withdraw an unnamed route from GableLBM (truck %s): %w", o.VehicleID, err)
		}
		slog.Warn("withdrew a route the dispatch board held that no plan named",
			"date", next.PlanDate, "vehicle", o.VehicleID, "route", o.RouteID,
			"was_live", res.Recalled, "stops", res.StopCount, "approved_by", who)
		next.BoardRepairs = append(next.BoardRepairs, BoardRepair{
			Kind: DivergenceOrphan, Action: RepairRecalled,
			VehicleID: o.VehicleID, RouteID: o.RouteID, BoardStatus: o.BoardStatus,
			StopCount: o.StopCount, OrderIDs: o.OrderIDs,
			At: now, By: who, Note: note,
		})
	}
	return nil
}

// recordReportedDivergences writes onto the new plan every divergence this
// ingest did NOT act on, so the artifact a dispatcher opens says what else was
// true of the date it was planned over.
//
// Ghosts are included with their own action: they were repaired on the plan
// that held them, where the tombstone lives, but "this re-plan corrected two
// stale claims first" is not discoverable from the new plan otherwise.
// Dispatched and unaddressable routes are recorded as reported and nothing
// more, which is exactly what happened to them.
func recordReportedDivergences(p *Plan, divs []BoardDivergence, at time.Time) {
	for _, d := range divs {
		action := RepairReported
		if d.Kind == DivergenceGhost {
			action = RepairTombstoned
		}
		if d.Kind == DivergenceOrphan {
			// Orphans are recorded by recallOrphans with what actually
			// happened to them; anything else would double-count.
			continue
		}
		p.BoardRepairs = append(p.BoardRepairs, BoardRepair{
			Kind: d.Kind, Action: action,
			VehicleID: d.VehicleID, VehicleName: d.VehicleName, RouteID: d.RouteID,
			BoardStatus: d.BoardStatus, StopCount: d.StopCount, OrderIDs: d.OrderIDs,
			PlanID: d.PlanID, At: at, By: systemRecaller, Note: d.Note,
		})
	}
}

// keysOf lists a set in a stable order, for a log line. Sorted, because a log
// field whose order changes run to run cannot be diffed between two incidents.
func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
