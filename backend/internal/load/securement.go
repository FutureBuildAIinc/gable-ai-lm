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

	// Strap count: the ruleset minimum, escalated until a 4" winch strap can
	// carry each share.
	n := required
	if n < 1 {
		n = 1
	}
	for int64(math.Ceil(float64(aggregate)/float64(n))) > wll4InWinchLbs {
		n++
	}

	positions, snapped := anchorPositions(minX, maxX, spacing, n)
	if len(positions) == 0 {
		return // unreachable: n ≥ 1 always yields at least one position
	}

	// The per-strap WLL share is derived from the straps ACTUALLY emitted, so
	// the sum of Strap.RequiredWLLLbs always meets MinAggregateWLLLbs. Deriving
	// it from a count the positions could not deliver is exactly how this plan
	// came to recommend a single 2,500 lb strap for a load whose own note
	// demanded 5,000 lb of aggregate WLL — see anchorPositions.
	perStrap := int64(math.Ceil(float64(aggregate) / float64(len(positions))))
	recommended := fmt.Sprintf("2\" ratchet strap (WLL %d lb)", wll2InRatchetLbs)
	if perStrap > wll2InRatchetLbs {
		recommended = fmt.Sprintf("4\" winch strap (WLL %d lb)", wll4InWinchLbs)
	}

	straps := make([]Strap, 0, len(positions))
	for i, pos := range positions {
		straps = append(straps, Strap{
			Number:         i + 1,
			PositionIn:     math.Round(pos*10) / 10,
			OverHeightIn:   loadHeightAt(plan.Placements, pos),
			RequiredWLLLbs: perStrap,
		})
	}

	anchorNote := fmt.Sprintf("Straps snapped to the bed's %.0f in anchor pitch (winch track / stake pockets).", spacing)
	if !snapped {
		// Say so rather than claim a snap that did not happen: the count is the
		// legal minimum and it wins, but the yard needs to know these positions
		// are approximate and to use the nearest real anchor to each.
		anchorNote = fmt.Sprintf(
			"This load spans fewer than %d of the bed's %.0f in anchors, so strap positions are spread across the load rather than snapped — use the nearest real anchor to each.",
			len(positions), spacing)
	}
	notes := []string{
		fmt.Sprintf("Aggregate WLL must be ≥ %.0f%% of cargo weight (%d lb) — %s.",
			rs.AggregateWLLFraction*100, aggregate, rs.Name),
		rs.Basis,
		anchorNote,
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

// anchorPositions chooses n tie-down positions across the load span. It returns
// EXACTLY n positions whenever n > 0, sorted front→back, and reports whether
// every one of them landed on a modeled bed anchor.
//
// Positions are spread evenly and snapped to the anchor grid (a multiple of
// spacing) whenever the span contains at least n distinct anchors — the normal
// case for a lumber load, and what the second return value reports as true.
// Collisions are resolved to the nearest free anchor so two straps never share
// one.
//
// When the span does NOT contain n anchors — a short, heavy article on a coarse
// anchor pitch, e.g. a 40 in pallet of block on a 24 in winch track — the
// positions are spread across the span WITHOUT snapping instead of dropping the
// surplus straps. The anchor pitch is a model of the deck (and is defaulted
// outright when the fleet profile carries none); the tie-down count is a legal
// minimum from the ruleset. Letting the model shorten the list under-secured
// the load twice over: fewer straps than the ruleset itself demanded, AND — far
// worse — an aggregate WLL below the ruleset minimum, because the per-strap
// share had been sized for the count that was never emitted. Two 40 in pallets
// weighing 10,000 lb came back with ONE 2,500 lb strap against a stated 5,000 lb
// minimum.
func anchorPositions(minX, maxX, spacing float64, n int) (positions []float64, snapped bool) {
	if n <= 0 {
		return nil, true
	}
	if spacing > 0 && maxX > minX {
		// Anchor slots that fall within the load span.
		lo := math.Ceil(minX/spacing) * spacing
		hi := math.Floor(maxX/spacing) * spacing
		var slots []float64
		for x := lo; x <= hi+1e-9; x += spacing {
			slots = append(slots, x)
		}
		if len(slots) >= n {
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
			return chosen, true
		}
	}
	return evenSpread(minX, maxX, n), false
}

// evenSpread places exactly n positions across [minX, maxX]: both ends plus an
// even interior pitch. A degenerate span puts them all at the centre — the
// count is still honoured, because it is the aggregate WLL that carries the
// load and the count is what divides it.
func evenSpread(minX, maxX float64, n int) []float64 {
	if n <= 0 {
		return nil
	}
	out := make([]float64, 0, n)
	if n == 1 || maxX <= minX {
		mid := (minX + maxX) / 2
		for i := 0; i < n; i++ {
			out = append(out, mid)
		}
		return out
	}
	for i := 0; i < n; i++ {
		out = append(out, minX+(maxX-minX)*float64(i)/float64(n-1))
	}
	return out
}

// nearestFreeSlot returns idx if free, else the closest free index searching
// outward. Assumes at least one slot is free (guaranteed: n ≤ len(slots)).
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
