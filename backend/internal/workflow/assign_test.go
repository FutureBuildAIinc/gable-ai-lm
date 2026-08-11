// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/routing"
)

func capPtr(n int) *int { return &n }

// TestSweepAssignVolumeCap verifies a truck is "full" on bed volume even when it
// is nowhere near its weight capacity (T2-2): two bulky-but-light stops cannot
// share a single small-volume truck.
func TestSweepAssignVolumeCap(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0
	vehicles := []gable.Vehicle{
		{ID: "truck-1", Name: "Truck 1", VehicleType: "FLATBED", CapacityWeightLbs: capPtr(100000)},
	}
	// Two stops near the depot, light (weight never binds) but bulky.
	stops := []routing.Stop{
		{OrderID: "a", Lat: 49.01, Lng: -119.01, WeightLbs: 100, VolumeCuFt: 40},
		{OrderID: "b", Lat: 49.02, Lng: -119.02, WeightLbs: 100, VolumeCuFt: 40},
	}
	// Usable bed volume of 50 ft³ — one 40 ft³ stop fits, two do not.
	volCap := map[string]float64{"truck-1": 50}

	loads, unassigned := sweepAssign(vehicles, stops, depotLat, depotLng, volCap)
	if len(loads) != 1 || len(loads[0].Stops) != 1 {
		t.Fatalf("expected one truck with one stop under the volume cap, got %d loads", len(loads))
	}
	if len(unassigned) != 1 {
		t.Fatalf("expected the second bulky stop to be unassigned on volume, got %d unassigned", len(unassigned))
	}

	// With no volume cap, both stops fit (weight + count + shift all allow it).
	loads2, unassigned2 := sweepAssign(vehicles, stops, depotLat, depotLng, nil)
	if len(loads2) != 1 || len(loads2[0].Stops) != 2 || len(unassigned2) != 0 {
		t.Fatalf("without a volume cap both stops should share the truck, got %d stops / %d unassigned",
			len(loads2[0].Stops), len(unassigned2))
	}
}
