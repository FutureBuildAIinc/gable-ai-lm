// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"fmt"
	"math"
	"sort"
)

// SolveSequencedBundles packs a multi-stop truck LIFO the way a lumber yard
// actually loads a flatbed: in vertical TIERS. Stops are processed in REVERSE
// route order, each stop's material packed as a tier across the full bed
// footprint — so the last delivery sits on the bottom and the first delivery
// is loaded last, on top, where it comes off first.
//
// Within a tier, same-SKU units are arranged as banded lumber bundles
// (`columns` boards across × `layers` boards high). Every board is an
// individual Placement carrying its order, stop and 1-based pack Step, so the
// 3D view renders realistic bundles and the yard app can walk the load
// piece by piece.
//
// Deterministic for a given input.
func SolveSequencedBundles(v Vehicle, stops []StopItems) Plan {
	plan := Plan{
		GableVehicleID: v.GableVehicleID,
		Placements:     []Placement{},
		AxleLoads:      []AxleLoad{},
		Unplaced:       []string{},
	}

	// Reverse route order: highest stop sequence loads first (bottom tier).
	ordered := make([]StopItems, len(stops))
	copy(ordered, stops)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].StopSequence > ordered[j].StopSequence
	})

	bedH := v.BedHeightIn
	if bedH <= 0 {
		bedH = math.Inf(1) // open bed: no configured height cap
	}

	// Volume budget (T2-2): the bed envelope alone over-states what a truck can
	// really carry, because banding gaps, dunnage and irregular stock are not in
	// the bounding boxes. Enforce the usable-volume budget as a hard cap
	// alongside the geometry so a high-volume / low-weight load is capped by
	// space, not just by weight. The assignment step already sizes trucks with
	// the same UsableBedVolumeCuFt budget, so packing and assignment agree.
	usableVol := UsableBedVolumeCuFt(v.BedLengthIn, v.BedWidthIn, v.BedHeightIn)
	var placedVol float64

	step := 0
	tierBase := 0.0 // bottom of the current stop's tier
	// SKU of the non-stackable article sealing the top of the load, if any.
	// Nothing may be tiered above it, so later (earlier-stop) tiers cannot be
	// built — they are reported unplaced rather than silently piled on top.
	bedSealedBy := ""

	for _, stop := range ordered {
		if bedSealedBy != "" {
			for _, it := range stop.Items {
				plan.Unplaced = append(plan.Unplaced,
					fmt.Sprintf("%s ×%d (cannot stack on non-stackable %s)", it.SKU, itemQty(it), bedSealedBy))
			}
			continue
		}

		tierStart := len(plan.Placements)
		items := make([]Item, len(stop.Items))
		copy(items, stop.Items)
		// Heaviest SKU first within the tier: stability + determinism.
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].WeightLbs != items[j].WeightLbs {
				return items[i].WeightLbs > items[j].WeightLbs
			}
			return items[i].SKU < items[j].SKU
		})

		// Shelf cursor across the bed footprint at the current level. When the
		// footprint fills, the level rises within the tier (a sub-level).
		level := tierBase
		cursorX, cursorY, rowDepth, levelMaxH := 0.0, 0.0, 0.0, 0.0
		tierTop := tierBase
		// levelSealedBy: a non-stackable article sits at the current level, so
		// the level may not rise over it. tierSealedBy: one sits anywhere in
		// this tier, so the next tier may not be built on top of it.
		levelSealedBy, tierSealedBy := "", ""

		for _, it := range items {
			qty := itemQty(it)
			if it.LengthIn <= 0 || it.WidthIn <= 0 || it.HeightIn <= 0 {
				plan.Unplaced = append(plan.Unplaced, fmt.Sprintf("%s ×%d (no geometry)", it.SKU, qty))
				continue
			}
			if it.LengthIn > v.BedLengthIn || it.WidthIn > v.BedWidthIn {
				plan.Unplaced = append(plan.Unplaced, fmt.Sprintf("%s ×%d (too large for bed)", it.SKU, qty))
				continue
			}

			remaining := qty
			unitVol := itemVolumeCuFt(it)
			for remaining > 0 {
				// Hard volume cap: how many more units of this article the
				// usable-volume budget still has room for.
				volRoom := remaining
				if usableVol > 0 && unitVol > 0 {
					volRoom = int(math.Floor((usableVol - placedVol) / unitVol))
					if volRoom <= 0 {
						plan.Unplaced = append(plan.Unplaced,
							fmt.Sprintf("%s ×%d (bed volume full)", it.SKU, remaining))
						break
					}
				}

				headroom := bedH - level
				cols, layers := bundleShape(remaining, it, v, headroom)
				if cols == 0 {
					// No headroom at this level — try the next level up.
					if levelMaxH > 0 {
						if levelSealedBy != "" {
							plan.Unplaced = append(plan.Unplaced,
								fmt.Sprintf("%s ×%d (cannot stack on non-stackable %s)", it.SKU, remaining, levelSealedBy))
							break
						}
						level += levelMaxH
						cursorX, cursorY, rowDepth, levelMaxH = 0, 0, 0, 0
						continue
					}
					plan.Unplaced = append(plan.Unplaced, fmt.Sprintf("%s ×%d (truck full)", it.SKU, remaining))
					break
				}
				bw := float64(cols) * it.WidthIn
				bh := float64(layers) * it.HeightIn

				// Wrap to a new row when out of width.
				if cursorY+bw > v.BedWidthIn {
					cursorX += rowDepth
					cursorY = 0
					rowDepth = 0
				}
				// Out of bed length → raise to the next level within the tier.
				if cursorX+it.LengthIn > v.BedLengthIn {
					if levelMaxH <= 0 {
						// Nothing placed at this level and it already overflows.
						plan.Unplaced = append(plan.Unplaced, fmt.Sprintf("%s ×%d (truck full)", it.SKU, remaining))
						break
					}
					if levelSealedBy != "" {
						plan.Unplaced = append(plan.Unplaced,
							fmt.Sprintf("%s ×%d (cannot stack on non-stackable %s)", it.SKU, remaining, levelSealedBy))
						break
					}
					level += levelMaxH
					cursorX, cursorY, rowDepth, levelMaxH = 0, 0, 0, 0
					continue
				}

				// Lay the bundle board-by-board: bottom layer up, left to right —
				// the physical order a packer follows.
				count := cols * layers
				if count > remaining {
					count = remaining
				}
				if count > volRoom {
					count = volRoom
				}
				placed := 0
				for layer := 0; layer < layers && placed < count; layer++ {
					for col := 0; col < cols && placed < count; col++ {
						step++
						plan.Placements = append(plan.Placements, Placement{
							ItemID:       it.ProductID,
							SKU:          it.SKU,
							X:            cursorX,
							Y:            cursorY + float64(col)*it.WidthIn,
							Z:            level + float64(layer)*it.HeightIn,
							LengthIn:     it.LengthIn,
							WidthIn:      it.WidthIn,
							HeightIn:     it.HeightIn,
							WeightLbs:    it.WeightLbs,
							AxleGroup:    nearestAxle(v.Axles, cursorX+it.LengthIn/2),
							OrderID:      stop.OrderID,
							StopSequence: stop.StopSequence,
							Step:         step,
						})
						placed++
					}
				}
				remaining -= placed
				placedVol += float64(placed) * unitVol
				if placed > 0 && !it.Stackable {
					// Nothing may be laid over this article: seal the level
					// (no rise within this tier) and the tier (no tier on top).
					levelSealedBy, tierSealedBy = it.SKU, it.SKU
				}

				cursorY += bw
				if it.LengthIn > rowDepth {
					rowDepth = it.LengthIn
				}
				if bh > levelMaxH {
					levelMaxH = bh
				}
				if level+bh > tierTop {
					tierTop = level + bh
				}
			}
		}

		// Center this tier along the bed so the cargo mass sits between the
		// axles instead of biased to the nose (steer-axle relief).
		maxX := 0.0
		for i := tierStart; i < len(plan.Placements); i++ {
			if end := plan.Placements[i].X + plan.Placements[i].LengthIn; end > maxX {
				maxX = end
			}
		}
		if shift := (v.BedLengthIn - maxX) / 2; shift > 0 {
			for i := tierStart; i < len(plan.Placements); i++ {
				plan.Placements[i].X += shift
				plan.Placements[i].AxleGroup = nearestAxle(v.Axles, plan.Placements[i].X+plan.Placements[i].LengthIn/2)
			}
		}

		// Next (earlier) stop stacks on top of this tier — unless this tier
		// contains a non-stackable article, in which case the load is sealed
		// here and no further tier may be built.
		if tierTop > tierBase {
			tierBase = tierTop
		}
		if tierSealedBy != "" {
			bedSealedBy = tierSealedBy
		}
	}

	computeAxleLoads(&plan, v)
	computeVolume(&plan, v)
	for _, p := range plan.Placements {
		if top := p.Z + p.HeightIn; top > plan.MaxLoadHeightIn {
			plan.MaxLoadHeightIn = top
		}
	}
	computeSecurement(&plan, v)
	return plan
}

// itemQty is the placeable unit count for an item; a missing or non-positive
// quantity is treated as one unit.
func itemQty(it Item) int {
	if it.Quantity <= 0 {
		return 1
	}
	return it.Quantity
}

// bundleShape picks the banded-unit cross-section for qty boards of an item:
// `cols` boards across × `layers` high, aiming for the flat, wide unit a yard
// bands (height capped at ~30″ per bundle) while respecting bed width and the
// remaining headroom. Returns (0, 0) when not even a single board fits the
// headroom.
//
// A non-stackable article (Item.Stackable false — natural-stone slab, crated
// window, 6x6 PT post, palletized goods) is never banded more than ONE unit
// high, however much headroom remains; it is widened across the bed instead.
func bundleShape(qty int, it Item, v Vehicle, headroomIn float64) (cols, layers int) {
	const maxBundleHeightIn = 30.0

	maxCols := int(v.BedWidthIn / it.WidthIn)
	if maxCols < 1 {
		return 0, 0
	}
	// maxBundleHeightIn caps how tall a BAND may be; headroomIn caps what still
	// fits above the current level. An article taller than the band cap is not
	// unplaceable — it simply is never banded, so it is laid one unit high
	// whenever the remaining headroom takes it. Conflating the two caps made
	// every article over 30 in tall (a 40 in stone step, a crated window) report
	// "truck full" on an empty bed.
	maxLayers := int(math.Min(maxBundleHeightIn, headroomIn) / it.HeightIn)
	if maxLayers < 1 {
		if it.HeightIn > headroomIn {
			return 0, 0 // genuinely no headroom left for even one unit
		}
		maxLayers = 1
	}
	if !it.Stackable {
		maxLayers = 1
	}

	// Square-ish cross-section: cols*W ≈ layers*H ⇒ cols ≈ sqrt(qty·H/W).
	cols = int(math.Round(math.Sqrt(float64(qty) * it.HeightIn / it.WidthIn)))
	if cols < 1 {
		cols = 1
	}
	if cols > maxCols {
		cols = maxCols
	}
	layers = (qty + cols - 1) / cols
	if layers > maxLayers {
		layers = maxLayers
		// Height-capped: widen the bundle to carry more per bundle.
		needed := (qty + layers - 1) / layers
		if needed > maxCols {
			needed = maxCols
		}
		if needed > cols {
			cols = needed
		}
	}
	return cols, layers
}
