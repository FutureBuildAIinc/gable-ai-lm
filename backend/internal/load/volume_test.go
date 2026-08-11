// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"math"
	"strings"
	"testing"
)

func TestUsableBedVolumeCuFt(t *testing.T) {
	// 96×96×48 in = 256 ft³ raw; usable = 256 × packEfficiency.
	got := UsableBedVolumeCuFt(96, 96, 48)
	want := 96.0 * 96.0 * 48.0 / 1728.0 * packEfficiency
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("usable volume = %.4f, want %.4f", got, want)
	}
	if UsableBedVolumeCuFt(0, 96, 48) != 0 {
		t.Error("zero dimension should yield zero usable volume")
	}
}

// TestShelfSolverVolumeCap verifies the solver enforces the bed volume budget as
// a hard constraint: a high-volume / low-weight load is capped by space, not by
// weight, and the overflow is flagged (not packed as if it fit).
func TestShelfSolverVolumeCap(t *testing.T) {
	v := Vehicle{
		GableVehicleID: "veh-vol",
		BedLengthIn:    96,
		BedWidthIn:     96,
		BedHeightIn:    48,
		GVWRLbs:        26000,
		TareWeightLbs:  10000,
		Axles: []Axle{
			{AxleNumber: 1, MaxWeightLbs: 12000, PositionFromFrontIn: 0, AxleType: "STEER"},
			{AxleNumber: 2, MaxWeightLbs: 20000, PositionFromFrontIn: 80, AxleType: "DRIVE"},
		},
	}
	// usable ≈ 166.4 ft³; each unit is 53.33 ft³ → only 3 fit by volume even
	// though the bed footprint physically has room for a 4th. Light (1 lb) so
	// weight never binds.
	items := []Item{
		{ProductID: "stone", SKU: "STEP-SLAB", Quantity: 4, LengthIn: 48, WidthIn: 48, HeightIn: 40, WeightLbs: 1, Stackable: false},
	}
	plan := NewShelfSolver().Solve(v, items)

	if len(plan.Placements) != 3 {
		t.Fatalf("expected 3 placements under the volume cap, got %d (unplaced %v)", len(plan.Placements), plan.Unplaced)
	}
	foundVolFlag := false
	for _, u := range plan.Unplaced {
		if strings.Contains(u, "bed volume full") {
			foundVolFlag = true
		}
	}
	if !foundVolFlag {
		t.Errorf("expected a 'bed volume full' unplaced flag, got %v", plan.Unplaced)
	}
	if plan.UsableVolumeCuFt <= 0 || plan.CargoVolumeCuFt <= 0 || plan.VolumeStatus == "" {
		t.Errorf("expected volume metrics on the plan, got usable=%.1f cargo=%.1f status=%q",
			plan.UsableVolumeCuFt, plan.CargoVolumeCuFt, plan.VolumeStatus)
	}
	if plan.CargoVolumeCuFt > plan.UsableVolumeCuFt+1e-6 {
		t.Errorf("placed cargo volume %.1f exceeded the usable budget %.1f", plan.CargoVolumeCuFt, plan.UsableVolumeCuFt)
	}
}

// TestNoGeometryItemFlaggedNotZeroBoxed verifies a no-geometry item is flagged
// rather than packed as a zero-size box.
func TestNoGeometryItemFlaggedNotZeroBoxed(t *testing.T) {
	v := testVehicle()
	items := []Item{
		{ProductID: "p1", SKU: "NO-GEOM", Quantity: 2, WeightLbs: 50},
	}
	plan := NewShelfSolver().Solve(v, items)
	if len(plan.Placements) != 0 {
		t.Fatalf("no-geometry item should not be placed, got %d placements", len(plan.Placements))
	}
	if len(plan.Unplaced) == 0 {
		t.Fatal("expected the no-geometry item to be flagged unplaced")
	}
}

func TestSequencedComputesVolumeMetrics(t *testing.T) {
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x4", Quantity: 40, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true},
		}},
	}
	plan := SolveSequencedBundles(sequencedTestVehicle(), stops)
	if plan.UsableVolumeCuFt <= 0 || plan.CargoVolumeCuFt <= 0 {
		t.Fatalf("expected positive volume metrics, got usable=%.2f cargo=%.2f", plan.UsableVolumeCuFt, plan.CargoVolumeCuFt)
	}
	if plan.VolumeStatus == "" {
		t.Error("expected a volume status")
	}
}
