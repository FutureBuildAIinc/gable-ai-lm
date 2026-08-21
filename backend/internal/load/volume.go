// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import "math"

// packEfficiency is the fraction of a bed's raw bounding volume that real cargo
// can occupy once banding gaps, irregular shapes, walkways and tier air-space
// are accounted for. It turns the bed envelope into a usable volume budget so a
// high-volume / low-weight load (e.g. natural-stone slab steps) is capped by
// space, not just weight. Deliberately conservative.
const packEfficiency = 0.65

// itemVolumeCuFt is one unit's bounding-box volume in cubic feet. Zero when the
// item has no usable geometry (so a no-geometry item never consumes the budget).
func itemVolumeCuFt(it Item) float64 {
	if it.LengthIn <= 0 || it.WidthIn <= 0 || it.HeightIn <= 0 {
		return 0
	}
	return it.LengthIn * it.WidthIn * it.HeightIn / 1728.0
}

// rawBedVolumeCuFt is the bed's full bounding volume (L×W×H) in cubic feet.
func rawBedVolumeCuFt(v Vehicle) float64 {
	if v.BedLengthIn <= 0 || v.BedWidthIn <= 0 || v.BedHeightIn <= 0 {
		return 0
	}
	return v.BedLengthIn * v.BedWidthIn * v.BedHeightIn / 1728.0
}

// UsableBedVolumeCuFt is the volume budget a load may occupy: the bed bounding
// volume discounted by the packing-efficiency factor. Exposed so the assignment
// step can apply the same volume cap the packer uses.
//
// KNOWN GAP — the same number, two different constraints. At pack time this is
// one of TWO caps: the tier packer also has to fit real boxes inside the bed
// envelope, and that is the cap that actually bites. Measured on the default
// 288×96×96 flatbed (budget 998 ft³) across six representative lumber mixes at
// one, two and three stops, the largest load that packs with nothing dropped
// ranges from 63% to 99% of this budget, and every overflow is ReasonTruckFull
// (out of deck or headroom) rather than ReasonVolumeFull. Assignment, which has
// only this cap, can therefore hand a truck more cube than the packer can place.
//
// Derating this constant does NOT reconcile them, for three reasons:
//
//  1. the shortfall is mix-dependent (a flat OSB load reaches 98%, a mixed
//     framing load 67%) and not monotonic in stop count, so no scalar covers it;
//  2. derating to the worst case would under-fill EVERY truck by a third, which
//     puts more trucks on the road than the day needs — a real cost to a dealer,
//     traded for a rarer overflow;
//  3. this constant is also the packer's own hard cap, so lowering it makes the
//     packer drop cargo EARLIER — the opposite of the intent.
//
// Closing it properly means giving assignment the packer's second constraint:
// either trial-pack each candidate truck during assignment (which requires the
// line items and the fleet profile inside sweepAssign, and a re-sequence per
// candidate because tier packing is stop-order dependent), or model the tier
// geometry analytically (bed height ÷ per-stop tier height, row depth = longest
// board in the row). Both change the assignment contract; neither is a constant.
func UsableBedVolumeCuFt(bedLengthIn, bedWidthIn, bedHeightIn float64) float64 {
	if bedLengthIn <= 0 || bedWidthIn <= 0 || bedHeightIn <= 0 {
		return 0
	}
	return bedLengthIn * bedWidthIn * bedHeightIn / 1728.0 * packEfficiency
}

// computeVolume tallies the placed cargo volume against the bed's usable volume
// budget and records the utilization + status on the plan. It is a reporting +
// gating signal that complements the physical bed-envelope packing.
func computeVolume(plan *Plan, v Vehicle) {
	var cargo float64
	for _, p := range plan.Placements {
		if p.LengthIn <= 0 || p.WidthIn <= 0 || p.HeightIn <= 0 {
			continue
		}
		cargo += p.LengthIn * p.WidthIn * p.HeightIn / 1728.0
	}
	plan.BedVolumeCuFt = round2vol(rawBedVolumeCuFt(v))
	plan.UsableVolumeCuFt = round2vol(UsableBedVolumeCuFt(v.BedLengthIn, v.BedWidthIn, v.BedHeightIn))
	plan.CargoVolumeCuFt = round2vol(cargo)
	if plan.UsableVolumeCuFt > 0 {
		util := cargo / plan.UsableVolumeCuFt
		plan.VolumeUtilization = round3(util)
		plan.VolumeStatus = utilStatus(util)
	} else {
		plan.VolumeStatus = "PASS"
	}
}

func round2vol(f float64) float64 { return math.Round(f*100) / 100 }
