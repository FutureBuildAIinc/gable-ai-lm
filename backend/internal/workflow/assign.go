// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

// Sweep-based CVRP assignment for the guided workflow. The legacy routing
// module's FFD assignment optimizes purely for weight, which happily piles an
// entire day onto one big truck. Dispatchers split a day geographically:
// stops are swept by polar angle around the depot so each truck gets a
// coherent slice of the map, and a truck is "full" when any of cargo cap,
// stop count, or shift duration would be exceeded.

import (
	"math"
	"sort"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/routing"
)

const (
	// cargoUtilizationCap keeps headroom below the rated payload so the packed
	// load can still pass axle checks (cargo never sits perfectly distributed).
	cargoUtilizationCap = 0.80
	// LIFO tier packing stacks one tier per stop, so stop count is bounded by
	// what physically stacks on a flatbed (also matches real LBM route sizes).
	maxStopsPerTruck    = 3
	maxRouteDurationMin = 480.0 // one shift
	serviceMinPerStop   = 25.0  // unload time per stop
	assignAvgSpeedMph   = 35.0
)

// sweepAssign clusters stops by sweep angle and fills trucks (largest first)
// against cargo/stop/shift caps. A truck is also "full" when the next stop
// would exceed its usable bed volume (T2-2): high-volume / low-weight loads
// (e.g. natural-stone steps) max out on space before weight. volCapByVehicle
// maps a vehicle id to its usable bed volume in ft³; a zero/absent entry
// disables the volume cap for that truck. Returns per-truck loads (stops
// unsequenced) and any stops no truck could take.
func sweepAssign(vehicles []gable.Vehicle, stops []routing.Stop, depotLat, depotLng float64, volCapByVehicle map[string]float64) ([]routing.Load, []routing.Stop) {
	usable := make([]gable.Vehicle, 0, len(vehicles))
	for _, v := range vehicles {
		if v.CapacityWeightLbs != nil && *v.CapacityWeightLbs > 0 {
			usable = append(usable, v)
		}
	}
	sort.SliceStable(usable, func(i, j int) bool {
		ci, cj := *usable[i].CapacityWeightLbs, *usable[j].CapacityWeightLbs
		if ci != cj {
			return ci > cj
		}
		return usable[i].ID < usable[j].ID
	})

	// Sweep order: polar angle around the depot, ties by distance then id.
	swept := make([]routing.Stop, len(stops))
	copy(swept, stops)
	sort.SliceStable(swept, func(i, j int) bool {
		ai := math.Atan2(swept[i].Lat-depotLat, swept[i].Lng-depotLng)
		aj := math.Atan2(swept[j].Lat-depotLat, swept[j].Lng-depotLng)
		if ai != aj {
			return ai < aj
		}
		return swept[i].OrderID < swept[j].OrderID
	})

	var loads []routing.Load
	var unassigned []routing.Stop
	vi := 0
	var cur *routing.Load
	var curCap float64
	var curVolCap float64 // usable bed volume ft³ (0 ⇒ no volume cap)
	var curVol float64    // volume committed to the current truck
	var curDur float64
	var lastLat, lastLng float64

	openNext := func() bool {
		if vi >= len(usable) {
			return false
		}
		v := usable[vi]
		vi++
		loads = append(loads, routing.Load{
			VehicleID:         v.ID,
			VehicleName:       v.Name,
			CapacityWeightLbs: *v.CapacityWeightLbs,
		})
		cur = &loads[len(loads)-1]
		curCap = float64(*v.CapacityWeightLbs) * cargoUtilizationCap
		curVolCap = volCapByVehicle[v.ID]
		curVol = 0
		curDur = 0
		lastLat, lastLng = depotLat, depotLng
		return true
	}

	for _, s := range swept {
		placed := false
		for {
			if cur == nil && !openNext() {
				break
			}
			legMin := routing.HaversineMiles(lastLat, lastLng, s.Lat, s.Lng) / assignAvgSpeedMph * 60.0
			fitsVolume := curVolCap <= 0 || curVol+s.VolumeCuFt <= curVolCap
			fits := cur.TotalWeightLbs+s.WeightLbs <= curCap &&
				fitsVolume &&
				len(cur.Stops) < maxStopsPerTruck &&
				curDur+legMin+serviceMinPerStop <= maxRouteDurationMin
			// A stop bigger than any cap still goes alone on an empty truck if it
			// fits the raw payload (better one hot truck than a lost order). The
			// physical packer flags any genuine bed overflow as unplaced.
			if !fits && len(cur.Stops) == 0 && s.WeightLbs <= float64(cur.CapacityWeightLbs) {
				fits = true
			}
			if fits {
				cur.Stops = append(cur.Stops, s)
				cur.TotalWeightLbs += s.WeightLbs
				curVol += s.VolumeCuFt
				curDur += legMin + serviceMinPerStop
				lastLat, lastLng = s.Lat, s.Lng
				placed = true
				break
			}
			// Truck full — move to the next one.
			cur = nil
		}
		if !placed {
			unassigned = append(unassigned, s)
		}
	}

	// Drop trucks that received nothing (shouldn't happen, but be safe).
	nonEmpty := loads[:0]
	for _, l := range loads {
		if len(l.Stops) > 0 {
			nonEmpty = append(nonEmpty, l)
		}
	}
	return nonEmpty, unassigned
}
