// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"reflect"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

func capPtr(v int) *int { return &v }

func totalStops(loads []Load) int {
	n := 0
	for _, l := range loads {
		n += len(l.Stops)
	}
	return n
}

// TestAssignLoadsRespectsCapacity verifies every load stays within its
// vehicle's capacity and all fitting stops are placed.
func TestAssignLoadsRespectsCapacity(t *testing.T) {
	vehicles := []gable.Vehicle{
		{ID: "v-small", Name: "Small", CapacityWeightLbs: capPtr(10000)},
		{ID: "v-big", Name: "Big", CapacityWeightLbs: capPtr(20000)},
	}
	stops := []Stop{
		{OrderID: "o1", WeightLbs: 8000},
		{OrderID: "o2", WeightLbs: 7000},
		{OrderID: "o3", WeightLbs: 6000},
		{OrderID: "o4", WeightLbs: 5000},
	}

	loads, unassigned := assignLoads(vehicles, stops)

	if len(unassigned) != 0 {
		t.Fatalf("expected all stops assigned, got %d unassigned", len(unassigned))
	}
	if got := totalStops(loads); got != len(stops) {
		t.Fatalf("expected %d stops across loads, got %d", len(stops), got)
	}
	for _, l := range loads {
		if l.TotalWeightLbs > float64(l.CapacityWeightLbs) {
			t.Errorf("load %s over capacity: %.0f > %d", l.VehicleID, l.TotalWeightLbs, l.CapacityWeightLbs)
		}
	}
}

// TestAssignLoadsOversizedStop verifies a stop heavier than any vehicle is
// returned as unassigned rather than overloading a truck.
func TestAssignLoadsOversizedStop(t *testing.T) {
	vehicles := []gable.Vehicle{
		{ID: "v1", Name: "Truck", CapacityWeightLbs: capPtr(10000)},
	}
	stops := []Stop{
		{OrderID: "fits", WeightLbs: 4000},
		{OrderID: "huge", WeightLbs: 15000},
	}

	loads, unassigned := assignLoads(vehicles, stops)

	if len(unassigned) != 1 || unassigned[0].OrderID != "huge" {
		t.Fatalf("expected 'huge' unassigned, got %+v", unassigned)
	}
	if totalStops(loads) != 1 {
		t.Fatalf("expected 1 stop assigned, got %d", totalStops(loads))
	}
}

// TestAssignLoadsSkipsZeroCapacity verifies vehicles with nil/zero capacity are
// ignored, and empty loads are dropped.
func TestAssignLoadsSkipsZeroCapacity(t *testing.T) {
	vehicles := []gable.Vehicle{
		{ID: "no-cap", Name: "Unknown"},
		{ID: "zero", Name: "Zero", CapacityWeightLbs: capPtr(0)},
		{ID: "real", Name: "Real", CapacityWeightLbs: capPtr(5000)},
	}
	stops := []Stop{{OrderID: "o1", WeightLbs: 3000}}

	loads, unassigned := assignLoads(vehicles, stops)

	if len(unassigned) != 0 {
		t.Fatalf("expected no unassigned, got %d", len(unassigned))
	}
	if len(loads) != 1 || loads[0].VehicleID != "real" {
		t.Fatalf("expected single load on 'real', got %+v", loads)
	}
}

// TestAssignLoadsDeterministic verifies identical inputs (in any order) produce
// identical output.
func TestAssignLoadsDeterministic(t *testing.T) {
	vehicles := []gable.Vehicle{
		{ID: "v-big", Name: "Big", CapacityWeightLbs: capPtr(20000)},
		{ID: "v-small", Name: "Small", CapacityWeightLbs: capPtr(10000)},
	}
	stops := []Stop{
		{OrderID: "o3", WeightLbs: 6000},
		{OrderID: "o1", WeightLbs: 8000},
		{OrderID: "o4", WeightLbs: 5000},
		{OrderID: "o2", WeightLbs: 7000},
	}

	first, _ := assignLoads(vehicles, stops)

	// Shuffle inputs; FFD sorts internally so result must match.
	vehiclesShuffled := []gable.Vehicle{vehicles[1], vehicles[0]}
	stopsShuffled := []Stop{stops[1], stops[3], stops[0], stops[2]}
	second, _ := assignLoads(vehiclesShuffled, stopsShuffled)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("assignLoads not deterministic:\n first=%+v\nsecond=%+v", first, second)
	}
}
