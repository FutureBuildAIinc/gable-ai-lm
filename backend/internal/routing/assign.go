// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"sort"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// assignLoads bin-packs stops onto vehicles by weight capacity using a
// deterministic First-Fit-Decreasing (FFD) heuristic. Vehicles are sorted by
// capacity descending (ties by ID); stops by weight descending (ties by
// OrderID). Each stop is placed in the first load with enough remaining
// capacity; stops that fit no vehicle are returned as unassigned. Loads that
// end up empty are dropped. This is the swappable optimizer seam — a real CVRP
// solver can replace it without touching callers.
func assignLoads(vehicles []gable.Vehicle, stops []Stop) (loads []Load, unassigned []Stop) {
	// Only vehicles with a positive capacity can carry anything.
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

	sorted := make([]Stop, len(stops))
	copy(sorted, stops)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].WeightLbs != sorted[j].WeightLbs {
			return sorted[i].WeightLbs > sorted[j].WeightLbs
		}
		return sorted[i].OrderID < sorted[j].OrderID
	})

	// One bin per usable vehicle, tracking remaining capacity.
	loads = make([]Load, len(usable))
	remaining := make([]float64, len(usable))
	for i, v := range usable {
		loads[i] = Load{
			VehicleID:         v.ID,
			VehicleName:       v.Name,
			CapacityWeightLbs: *v.CapacityWeightLbs,
		}
		remaining[i] = float64(*v.CapacityWeightLbs)
	}

	for _, s := range sorted {
		placed := false
		for i := range loads {
			if s.WeightLbs <= remaining[i] {
				loads[i].Stops = append(loads[i].Stops, s)
				loads[i].TotalWeightLbs += s.WeightLbs
				remaining[i] -= s.WeightLbs
				placed = true
				break
			}
		}
		if !placed {
			unassigned = append(unassigned, s)
		}
	}

	// Drop loads that received no stops.
	nonEmpty := loads[:0]
	for _, l := range loads {
		if len(l.Stops) > 0 {
			nonEmpty = append(nonEmpty, l)
		}
	}
	loads = nonEmpty

	return loads, unassigned
}

// assignDrivers attaches a driver to each load round-robin. ACTIVE drivers are
// preferred and sorted by ID for determinism; if there are fewer drivers than
// loads they are reused. Loads are mutated in place. With no usable drivers,
// driver fields are left empty (write-back will then fail loudly upstream).
func assignDrivers(drivers []gable.Driver, loads []Load) {
	usable := make([]gable.Driver, 0, len(drivers))
	for _, d := range drivers {
		if d.ID != "" && (d.Status == "" || d.Status == "ACTIVE") {
			usable = append(usable, d)
		}
	}
	if len(usable) == 0 {
		return
	}
	sort.SliceStable(usable, func(i, j int) bool { return usable[i].ID < usable[j].ID })

	for i := range loads {
		d := usable[i%len(usable)]
		loads[i].DriverID = d.ID
		loads[i].DriverName = d.Name
	}
}
