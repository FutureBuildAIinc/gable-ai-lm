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

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
)

// divergenceRank orders a report the way a human reads it: what we repaired,
// what needs a decision, then what is merely context. It also makes every
// refusal sentence and every JSON body deterministic, which is what lets them
// be asserted at all.
var divergenceRank = map[string]int{
	DivergenceGhost:         0,
	DivergenceOrphan:        1,
	DivergenceDispatched:    2,
	DivergenceUnaddressable: 3,
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
	Note   string `json:"note"`
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
	// CANCELLED rows, which is how "did my recall land?" is answered.
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

// boardView is one read of the board, indexed the two ways every caller needs.
//
// live and dispatched are keyed by vehicle because the recall key is
// (vehicle_id, scheduled_date) and never a route id: an id captured at push
// time either names nothing by the time it matters or names a route that now
// belongs to somebody else. Routes with no vehicle appear in neither map and
// are held separately, because they cannot be addressed at all.
type boardView struct {
	date       string
	routes     []gable.BoardRoute
	live       map[string]gable.BoardRoute
	dispatched map[string]gable.BoardRoute
	unnamed    []gable.BoardRoute
}

// newBoardView indexes a board read.
//
// A truck can legitimately appear twice: the ERP's replace path only deletes
// DRAFT/SCHEDULED rows, so a truck that has already departed on one route can
// acquire a fresh SCHEDULED one. Live therefore wins for the reclaimable
// question while the departed row is still recorded, rather than one silently
// masking the other.
func newBoardView(date string, routes []gable.BoardRoute) boardView {
	v := boardView{
		date:       date,
		routes:     routes,
		live:       map[string]gable.BoardRoute{},
		dispatched: map[string]gable.BoardRoute{},
	}
	for _, r := range routes {
		switch {
		case r.Live() && r.VehicleID == "":
			v.unnamed = append(v.unnamed, r)
		case r.Live():
			v.live[r.VehicleID] = r
		case r.Dispatched() && r.VehicleID != "":
			v.dispatched[r.VehicleID] = r
		}
	}
	return v
}

// holds reports whether the board has ANY route for this truck that is not
// cancelled — live or already gone out.
//
// It is the ghost test, and it is deliberately wider than live(): a ledger
// entry whose board row is IN_TRANSIT is not a ghost. That route is real, it is
// ours, and it has simply departed; tombstoning it would be this service
// writing down "we took this off the board" about a truck that is at that
// moment driving the run. A CANCELLED row, by contrast, means the route really
// is gone, so a ledger still claiming it IS a ghost — which is the case that
// answers "did my recall land?".
func (v boardView) holds(vehicleID string) bool {
	if _, ok := v.live[vehicleID]; ok {
		return true
	}
	_, ok := v.dispatched[vehicleID]
	return ok
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

// backedLivePlans is "which plans does a re-plan actually strand?", answered
// against the BOARD rather than against the ledgers alone.
//
// A plan counts only if it claims a truck the board really holds. A plan whose
// every claim is a ghost strands NOTHING — there is nothing on the dealer's
// board to leave behind — so gating a re-plan on it would demand an approval
// for routes that do not exist, and the recall that approval authorizes is
// keyed (vehicle, date), which means it would cancel whatever route those
// trucks acquire next.
//
// It is deliberately a pure function of a board read and a plan set, with no
// writes and no persistence, because BOTH asks in Ingest need the same answer
// and only the second one is allowed to write. A gate that could only reach
// this answer by repairing first would have to repair before the cheap ask, and
// the cheap ask runs outside the dispatch-date claim.
func (v boardView) backedLivePlans(all []*Plan) []*Plan {
	out := make([]*Plan, 0, len(all))
	for _, p := range all {
		for _, r := range liveRoutes(p) {
			if v.holds(r.VehicleID) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// backedLiveRoutes is the ledger entries of these plans that the board actually
// backs — the trucks a re-plan would really be taking off the dealer's board.
//
// It exists so a refusal names only routes that exist. Naming a ghost in the
// approval prompt is not a cosmetic slip: it asks a dispatcher to weigh
// cancelling a run that is not there, and the recall they would be approving is
// keyed (vehicle, date), so exercising it would cancel whatever route that
// truck acquires next.
func (v boardView) backedLiveRoutes(plans []*Plan) []LiveRoute {
	// Built on liveRoutesAcross rather than beside it, because that is exactly
	// what this change did to the ledger: what the ledger claims is now an
	// input to be filtered by the board, not an answer. A second traversal here
	// would be a second definition of "live", and the two would drift.
	out := []LiveRoute{}
	for _, r := range liveRoutesAcross(plans) {
		if v.holds(r.VehicleID) {
			out = append(out, r)
		}
	}
	return out
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

// reconcile compares the board against the ledgers and names every way they
// disagree. It reads only: nothing here writes, recalls or gates.
//
// The four kinds are produced by three passes because they are three different
// questions — "does the board back what we claim?", "does anything on the board
// belong to nobody?", and "what is on this date that no recall could ever
// reach?" — and collapsing them into one loop is how the asymmetry between a
// ghost and an orphan gets lost.
func reconcile(v boardView, plans []*Plan) []BoardDivergence {
	var out []BoardDivergence

	// Ghosts: our record is wrong about our own state.
	for _, p := range plans {
		for _, r := range liveRoutes(p) {
			if v.holds(r.VehicleID) {
				continue
			}
			out = append(out, BoardDivergence{
				Kind:        DivergenceGhost,
				VehicleID:   r.VehicleID,
				VehicleName: r.VehicleName,
				PlanID:      p.ID,
				Note: fmt.Sprintf("plan %s claims truck %s is live on %s, but the dispatch board holds no such route — the claim is stale and is being tombstoned",
					p.ID, nameOrID(r), v.date),
			})
		}
	}

	claimed := claimsOn(plans)

	// Orphans and departed trucks: the board holds something we do not name.
	for _, r := range v.routes {
		if r.VehicleID == "" || len(claimed[r.VehicleID]) > 0 {
			continue
		}
		switch {
		case r.Live():
			out = append(out, BoardDivergence{
				Kind:        DivergenceOrphan,
				VehicleID:   r.VehicleID,
				RouteID:     r.RouteID,
				BoardStatus: r.Status,
				StopCount:   r.StopCount,
				OrderIDs:    r.OrderIDs,
				Note: fmt.Sprintf("the dispatch board holds a %s route for truck %s on %s with %d stop(s) that no plan of ours names — it may have been created in GableLBM by a dispatcher, so it is NOT withdrawn without an approval",
					r.Status, r.VehicleID, v.date, r.StopCount),
			})
		case r.Dispatched():
			out = append(out, BoardDivergence{
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

	// Routes the recall key cannot name at all.
	for _, r := range v.unnamed {
		out = append(out, BoardDivergence{
			Kind:        DivergenceUnaddressable,
			RouteID:     r.RouteID,
			BoardStatus: r.Status,
			StopCount:   r.StopCount,
			OrderIDs:    r.OrderIDs,
			Note: fmt.Sprintf("the dispatch board holds a %s route on %s with no vehicle assigned — the recall key is (vehicle_id, scheduled_date), so nothing this service can send names it; it has to be resolved in GableLBM",
				r.Status, v.date),
		})
	}

	sortDivergences(out)
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
	divs := reconcile(view, live)
	board := view.routes
	if board == nil {
		// A bare JSON array, never null: "the board is empty" and "the read
		// failed" must not decode to the same value on the far side.
		board = []gable.BoardRoute{}
	}
	if divs == nil {
		divs = []BoardDivergence{}
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
