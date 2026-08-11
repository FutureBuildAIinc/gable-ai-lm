// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"fmt"
	"math"
	"sort"
)

// Working load limits for the strap types we recommend.
const (
	wll2InRatchetLbs = 3335 // 2" ratchet strap
	wll4InWinchLbs   = 5400 // 4" winch strap
)

// defaultAnchorSpacingIn is the modeled winch-track / stake-pocket pitch along a
// flatbed when the fleet profile does not specify one.
const defaultAnchorSpacingIn = 24.0

// computeSecurement derives the tie-down plan for a packed load:
//   - the ruleset (jurisdiction) sets the aggregate WLL fraction and the minimum
//     tie-down count by article length / weight / max-spacing;
//   - the per-strap WLL share escalates the strap count until a 4" winch strap
//     covers it;
//   - strap positions are spread across the load span and snapped to the nearest
//     modeled bed anchor so each lands on a real tie-down point;
//   - the rule basis is recorded on the output so the recommendation is auditable.
func computeSecurement(plan *Plan, v Vehicle) {
	if len(plan.Placements) == 0 {
		return
	}

	rs := resolveSecurementRuleset(v.SecurementJurisdiction)
	spacing := v.AnchorSpacingIn
	if spacing <= 0 {
		spacing = defaultAnchorSpacingIn
	}

	minX, maxX := math.Inf(1), 0.0
	var cargo float64
	for _, p := range plan.Placements {
		cargo += p.WeightLbs
		if p.X < minX {
			minX = p.X
		}
		if end := p.X + p.LengthIn; end > maxX {
			maxX = end
		}
	}
	span := maxX - minX
	spanFt := span / 12.0

	// Minimum tie-downs from the jurisdiction ruleset.
	required := rs.requiredTieDowns(spanFt, cargo)

	aggregate := int64(math.Ceil(cargo * rs.AggregateWLLFraction))

	// Per-strap WLL share; escalate the count until a 4" winch strap covers it
	// (never below the ruleset minimum).
	n := required
	if n < 1 {
		n = 1
	}
	perStrap := int64(math.Ceil(float64(aggregate) / float64(n)))
	for perStrap > wll4InWinchLbs {
		n++
		perStrap = int64(math.Ceil(float64(aggregate) / float64(n)))
	}
	recommended := fmt.Sprintf("2\" ratchet strap (WLL %d lb)", wll2InRatchetLbs)
	if perStrap > wll2InRatchetLbs {
		recommended = fmt.Sprintf("4\" winch strap (WLL %d lb)", wll4InWinchLbs)
	}

	// Optimize placement: even spread across the load span, snapped to anchors.
	positions := anchorPositions(minX, maxX, spacing, n)
	straps := make([]Strap, 0, len(positions))
	for i, pos := range positions {
		straps = append(straps, Strap{
			Number:         i + 1,
			PositionIn:     math.Round(pos*10) / 10,
			OverHeightIn:   loadHeightAt(plan.Placements, pos),
			RequiredWLLLbs: perStrap,
		})
	}

	notes := []string{
		fmt.Sprintf("Aggregate WLL must be ≥ %.0f%% of cargo weight (%d lb) — %s.",
			rs.AggregateWLLFraction*100, aggregate, rs.Name),
		rs.Basis,
		fmt.Sprintf("Straps snapped to the bed's %.0f in anchor pitch (winch track / stake pockets).", spacing),
		"Use edge protectors wherever a strap crosses a board edge.",
		"Tighten winches/ratchets after the first 50 miles and re-check at every stop.",
		"Load is tiered by stop — re-strap the remaining tiers after each delivery.",
	}

	plan.Securement = &Securement{
		CargoWeightLbs:     int64(math.Round(cargo)),
		MinAggregateWLLLbs: aggregate,
		Straps:             straps,
		RecommendedStrap:   recommended,
		Jurisdiction:       rs.Code,
		RulesetName:        rs.Name,
		RuleBasis:          rs.Basis,
		RequiredTieDowns:   required,
		AnchorSpacingIn:    spacing,
		Notes:              notes,
	}
}

// anchorPositions chooses n tie-down positions spread evenly across the load
// span and snapped to the modeled bed anchor grid (a multiple of spacing). The
// returned positions are unique and sorted; collisions are resolved to the
// nearest free anchor so two straps never share one anchor.
func anchorPositions(minX, maxX, spacing float64, n int) []float64 {
	if n <= 0 {
		return nil
	}
	if spacing <= 0 || maxX <= minX {
		// Degenerate span — stack everything at the centre.
		return []float64{(minX + maxX) / 2}
	}

	// Anchor slots that fall within the load span.
	lo := math.Ceil(minX/spacing) * spacing
	hi := math.Floor(maxX/spacing) * spacing
	var slots []float64
	for x := lo; x <= hi+1e-9; x += spacing {
		slots = append(slots, x)
	}
	if len(slots) == 0 {
		// Span shorter than one anchor cell — snap the centre to an anchor.
		mid := math.Round((minX+maxX)/2/spacing) * spacing
		return []float64{mid}
	}
	if n >= len(slots) {
		return slots // use every anchor in the span
	}

	used := make([]bool, len(slots))
	chosen := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		frac := 0.5
		if n > 1 {
			frac = float64(i) / float64(n-1)
		}
		idx := int(math.Round(frac * float64(len(slots)-1)))
		idx = nearestFreeSlot(used, idx)
		used[idx] = true
		chosen = append(chosen, slots[idx])
	}
	sort.Float64s(chosen)
	return chosen
}

// nearestFreeSlot returns idx if free, else the closest free index searching
// outward. Assumes at least one slot is free (guaranteed: n < len(slots)).
func nearestFreeSlot(used []bool, idx int) int {
	if idx < 0 {
		idx = 0
	}
	if idx >= len(used) {
		idx = len(used) - 1
	}
	if !used[idx] {
		return idx
	}
	for d := 1; d < len(used); d++ {
		if lo := idx - d; lo >= 0 && !used[lo] {
			return lo
		}
		if hi := idx + d; hi < len(used) && !used[hi] {
			return hi
		}
	}
	return idx
}

// loadHeightAt returns the tallest stack the strap crosses at position x.
func loadHeightAt(placements []Placement, x float64) float64 {
	h := 0.0
	for _, p := range placements {
		if p.X <= x && x <= p.X+p.LengthIn {
			if top := p.Z + p.HeightIn; top > h {
				h = top
			}
		}
	}
	return math.Round(h*10) / 10
}
