// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"fmt"
	"math"
	"sort"
)

// Solver computes a load plan for a vehicle and a set of items. Implementations
// must be deterministic so the same input always yields the same plan.
type Solver interface {
	Solve(v Vehicle, items []Item) Plan
}

// thresholds for axle/GVW utilization status.
const (
	warnUtilization = 0.90
	failUtilization = 1.00
)

// TieredSolver is THE placement engine. It satisfies the single-shot Solver
// interface (one bag of items, no delivery sequence) by packing that bag as a
// single tier through SolveSequencedBundles — the same multi-tier bundle packer
// the multi-stop /plan workflow uses.
//
// There used to be two engines: a "shelf" heuristic behind
// POST /api/v1/load/optimize and the tiered packer behind the /plan workflow.
// They produced materially different plans for the same truck, and the shelf
// heuristic could not physically stack at all — it raised a unit's Z while its
// Y cursor kept advancing, so a "second layer" floated beside its row with
// nothing beneath it. Worse, every physical-safety rule (stackability, bed
// envelope, volume budget) had to be implemented and fixed twice. There is now
// one engine, so a self-hoster wiring the documented endpoint gets exactly the
// plan the production workflow would build.
type TieredSolver struct{}

// NewTieredSolver returns the deterministic tier/bundle placement engine.
func NewTieredSolver() *TieredSolver { return &TieredSolver{} }

// ShelfSolver is the previous name of the single engine.
//
// Deprecated: the shelf heuristic is gone; this is an alias for TieredSolver
// kept so existing wiring keeps compiling. Use TieredSolver.
type ShelfSolver = TieredSolver

// NewShelfSolver returns the default deterministic solver.
//
// Deprecated: use NewTieredSolver.
func NewShelfSolver() *ShelfSolver { return NewTieredSolver() }

// Solve packs items onto the vehicle as one synthetic stop. Placements carry
// their 1-based pack Step; OrderID and StopSequence are left unset because a
// single-shot solve has no delivery sequence to express.
func (s *TieredSolver) Solve(v Vehicle, items []Item) Plan {
	return SolveSequencedBundles(v, []StopItems{{Items: items}})
}

// computeAxleLoads distributes cargo + tare weight across axles, sets each
// axle's status, the overall GVW status, total weight and balance score.
//
// The per-axle numbers it produces are ADVISORY (every AxleLoad carries
// Advisory=true); the package doc states precisely what the model does and does
// not account for. Total weight and the gross-vs-GVWR verdict are exact.
//
// A rating the fleet profile never supplied (zero axle rating, zero GVWR, no
// axles) yields StatusUnknown — never PASS — and is recorded on
// Plan.ProfileStatus/ProfileIssues.
func computeAxleLoads(plan *Plan, v Vehicle) {
	// distributeToAxles requires axles ordered front→back. Profiles deliver them
	// ordered by axle_number, which is not guaranteed to be position-monotonic
	// (a tag or trailer axle is easily numbered out of order), so sort a copy and
	// keep a map back to the caller's ordering for the output.
	axles, origToSorted := sortedAxles(v.Axles)

	// Cargo is distributed on its own so the position-split can be checked for
	// weight conservation before tare is folded in.
	cargoLoads := make([]float64, len(axles))
	var cargo float64
	for _, p := range plan.Placements {
		cargo += p.WeightLbs
		distributeToAxles(cargoLoads, axles, p.X+p.LengthIn/2, p.WeightLbs)
	}

	var issues []string
	if len(axles) == 0 {
		issues = append(issues, "fleet profile has no axles — per-axle load cannot be computed")
	} else if err := checkCargoConservation(cargoLoads, cargo); err != nil {
		// Fail loudly rather than silently under-reporting an axle: a lost
		// pound here is a pound the dispatcher never sees on a scale-fine check.
		issues = append(issues, "internal: "+err.Error())
	}

	loads := make([]float64, len(axles))
	// Distribute tare proportional to each axle's rated capacity (heavier-rated
	// axles carry more of the chassis). Falls back to even split.
	var totalRating int64
	for _, a := range axles {
		totalRating += a.MaxWeightLbs
	}
	for i, a := range axles {
		if totalRating > 0 {
			loads[i] += float64(v.TareWeightLbs) * float64(a.MaxWeightLbs) / float64(totalRating)
		} else if len(axles) > 0 {
			loads[i] += float64(v.TareWeightLbs) / float64(len(axles))
		}
		loads[i] += cargoLoads[i]
	}

	plan.TotalWeightLbs = int64(math.Round(cargo)) + v.TareWeightLbs

	worst := statusRank(StatusPass)
	if len(axles) == 0 {
		worst = statusRank(StatusUnknown)
	}
	// Balance is the spread of utilization ACROSS axles, so it is only meaningful
	// when every axle is rated: an unrated axle contributes no utilization, and
	// scoring the remainder would report a confident "perfectly balanced" load
	// for a truck we cannot judge at all.
	ratingsComplete := len(axles) > 0
	utils := make([]float64, 0, len(axles))
	for i, a := range v.Axles { // emit in the caller's (axle_number) order
		w := loads[origToSorted[i]]
		util, st := ratedStatus(w, a.MaxWeightLbs)
		if statusRank(st) > worst {
			worst = statusRank(st)
		}
		if st == StatusUnknown {
			ratingsComplete = false
			issues = append(issues, fmt.Sprintf("axle %d has no rated capacity — its load cannot be judged", a.AxleNumber))
		} else {
			utils = append(utils, util)
		}
		plan.AxleLoads = append(plan.AxleLoads, AxleLoad{
			AxleNumber:   a.AxleNumber,
			WeightLbs:    int64(math.Round(w)),
			MaxWeightLbs: a.MaxWeightLbs,
			Utilization:  round3(util),
			Status:       st,
			Advisory:     true,
		})
	}

	// Overall GVW: compare total to GVWR as well. A blank GVWR is unknown, not
	// a pass — it must never let an arbitrarily heavy load read as compliant.
	if v.GVWRLbs > 0 {
		gvwUtil := float64(plan.TotalWeightLbs) / float64(v.GVWRLbs)
		if statusRank(utilStatus(gvwUtil)) > worst {
			worst = statusRank(utilStatus(gvwUtil))
		}
	} else {
		issues = append(issues, "fleet profile has no GVWR — gross weight cannot be judged")
		if statusRank(StatusUnknown) > worst {
			worst = statusRank(StatusUnknown)
		}
	}
	plan.GVWStatus = overallStatus(worst)
	if ratingsComplete {
		plan.BalanceScore = round3(balanceScore(utils))
	} // else: not computable — left at 0 alongside ProfileIncomplete.

	plan.ProfileIssues = issues
	plan.ProfileStatus = ProfileComplete
	if len(issues) > 0 {
		plan.ProfileStatus = ProfileIncomplete
	}
}

// sortedAxles returns a copy of axles ordered front→back by PositionFromFrontIn
// (ties broken on AxleNumber for determinism) plus origToSorted, where
// origToSorted[i] is the sorted-slice index of the caller's i-th axle, so
// results can still be reported in the caller's (axle_number) order.
func sortedAxles(axles []Axle) (sorted []Axle, origToSorted []int) {
	order := make([]int, len(axles))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := axles[order[i]], axles[order[j]]
		if a.PositionFromFrontIn != b.PositionFromFrontIn {
			return a.PositionFromFrontIn < b.PositionFromFrontIn
		}
		return a.AxleNumber < b.AxleNumber
	})
	sorted = make([]Axle, len(axles))
	origToSorted = make([]int, len(axles))
	for sortedIdx, origIdx := range order {
		sorted[sortedIdx] = axles[origIdx]
		origToSorted[origIdx] = sortedIdx
	}
	return sorted, origToSorted
}

// checkCargoConservation verifies the per-axle split accounts for every pound of
// cargo. distributeToAxles must conserve weight; a mismatch means an axle was
// silently under- or double-counted and the plan cannot be trusted.
func checkCargoConservation(loads []float64, cargo float64) error {
	var sum float64
	for _, l := range loads {
		sum += l
	}
	tol := math.Max(1e-6, math.Abs(cargo)*1e-9)
	if math.Abs(sum-cargo) > tol {
		return fmt.Errorf("per-axle cargo %.4f lb does not sum to total cargo %.4f lb", sum, cargo)
	}
	return nil
}

// ratedStatus is the utilization and verdict for a load against an axle rating.
// A missing (≤ 0) rating is StatusUnknown with zero utilization — it must never
// collapse into a confident PASS.
func ratedStatus(load float64, ratingLbs int64) (float64, string) {
	if ratingLbs <= 0 {
		return 0, StatusUnknown
	}
	util := load / float64(ratingLbs)
	return util, utilStatus(util)
}

// distributeToAxles allocates weight at longitudinal position x to the two
// axles that bracket x (linear interpolation). Weight outside the axle span is
// assigned fully to the nearest end axle — see the package doc: real overhang
// levers are NOT modelled, which is one reason per-axle output is advisory.
//
// axles MUST already be sorted front→back by PositionFromFrontIn (see
// sortedAxles); an unsorted slice mis-attributes weight to the wrong axles.
func distributeToAxles(loads []float64, axles []Axle, x, weight float64) {
	n := len(axles)
	if n == 0 {
		return
	}
	if n == 1 {
		loads[0] += weight
		return
	}
	// Before the first axle.
	if x <= axles[0].PositionFromFrontIn {
		loads[0] += weight
		return
	}
	// After the last axle.
	if x >= axles[n-1].PositionFromFrontIn {
		loads[n-1] += weight
		return
	}
	for i := 0; i < n-1; i++ {
		a, b := axles[i], axles[i+1]
		if x >= a.PositionFromFrontIn && x <= b.PositionFromFrontIn {
			span := b.PositionFromFrontIn - a.PositionFromFrontIn
			if span <= 0 {
				loads[i] += weight
				return
			}
			frac := (x - a.PositionFromFrontIn) / span
			loads[i] += weight * (1 - frac)
			loads[i+1] += weight * frac
			return
		}
	}
}

func nearestAxle(axles []Axle, x float64) int {
	if len(axles) == 0 {
		return 0
	}
	best := axles[0]
	bestDist := math.Abs(x - axles[0].PositionFromFrontIn)
	for _, a := range axles[1:] {
		d := math.Abs(x - a.PositionFromFrontIn)
		if d < bestDist {
			bestDist = d
			best = a
		}
	}
	return best.AxleNumber
}

// utilStatus grades a utilization ratio. It is only meaningful for a KNOWN
// rating — callers with a rating that may be missing must use ratedStatus, or a
// zero rating silently reads as a confident PASS.
func utilStatus(util float64) string {
	switch {
	case util > failUtilization:
		return StatusFail
	case util >= warnUtilization:
		return StatusWarn
	default:
		return StatusPass
	}
}

// statusRank orders verdicts by severity for roll-up. UNKNOWN outranks FAIL:
// an over-limit load is at least a known quantity the dispatcher can fix,
// whereas an unrated profile means no verdict on this plan can be trusted.
func statusRank(s string) int {
	switch s {
	case StatusUnknown:
		return 3
	case StatusFail:
		return 2
	case StatusWarn:
		return 1
	default:
		return 0
	}
}

func rankStatus(r int) string {
	switch r {
	case 3:
		return StatusUnknown
	case 2:
		return StatusFail
	case 1:
		return StatusWarn
	default:
		return StatusPass
	}
}

// overallStatus collapses a severity rank onto the three-value PASS/WARN/FAIL
// enum that Plan.GVWStatus publishes. UNKNOWN collapses to FAIL — blocking, and
// never PASS; the reason is carried verbatim on Plan.ProfileStatus/ProfileIssues.
func overallStatus(rank int) string {
	if rank >= statusRank(StatusUnknown) {
		return StatusFail
	}
	return rankStatus(rank)
}

// balanceScore returns 1 minus the normalized spread of axle utilizations.
// A perfectly even load scores 1.0; large imbalance trends toward 0.
//
// Callers must only score a profile whose axles are ALL rated (see
// computeAxleLoads): an unrated axle has no utilization, so scoring what remains
// would report a confident 1.0 "perfectly balanced" for a truck that cannot be
// judged. With no utilizations at all the score is 0, not 1.
func balanceScore(utils []float64) float64 {
	if len(utils) == 0 {
		return 0
	}
	var sum float64
	for _, u := range utils {
		sum += u
	}
	mean := sum / float64(len(utils))
	if mean == 0 {
		return 1
	}
	var variance float64
	for _, u := range utils {
		d := u - mean
		variance += d * d
	}
	variance /= float64(len(utils))
	cv := math.Sqrt(variance) / mean // coefficient of variation
	score := 1 - cv
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score
}

func round3(f float64) float64 {
	return math.Round(f*1000) / 1000
}
