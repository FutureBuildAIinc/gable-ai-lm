// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"strings"
	"testing"
)

func sequencedTestVehicle() Vehicle {
	return Vehicle{
		GableVehicleID: "veh-1",
		BedLengthIn:    288,
		BedWidthIn:     96,
		BedHeightIn:    96,
		GVWRLbs:        33000,
		TareWeightLbs:  14000,
		Axles: []Axle{
			{AxleNumber: 1, MaxWeightLbs: 12000, PositionFromFrontIn: 0, AxleType: "STEER"},
			{AxleNumber: 2, MaxWeightLbs: 21000, PositionFromFrontIn: 240, AxleType: "DRIVE"},
		},
	}
}

func TestSolveSequencedBundlesLIFO(t *testing.T) {
	stops := []StopItems{
		{OrderID: "first-stop", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x4", Quantity: 20, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true},
		}},
		{OrderID: "last-stop", StopSequence: 2, Items: []Item{
			{ProductID: "p2", SKU: "2x6", Quantity: 20, LengthIn: 120, WidthIn: 5.5, HeightIn: 1.5, WeightLbs: 17, Stackable: true},
		}},
	}

	plan := SolveSequencedBundles(sequencedTestVehicle(), stops)

	if len(plan.Placements) != 40 {
		t.Fatalf("expected 40 placements, got %d (unplaced: %v)", len(plan.Placements), plan.Unplaced)
	}

	// LIFO tiers: the LAST stop packs FIRST (lowest steps, bottom tier); the
	// FIRST stop loads last, on top, where it comes off first.
	if got := plan.Placements[0]; got.OrderID != "last-stop" || got.Step != 1 {
		t.Errorf("first placement should be last-stop step 1, got order=%s step=%d", got.OrderID, got.Step)
	}
	var maxTopLast, minZFirst float64 = 0, 1e9
	for _, p := range plan.Placements {
		switch p.OrderID {
		case "last-stop":
			if top := p.Z + p.HeightIn; top > maxTopLast {
				maxTopLast = top
			}
		case "first-stop":
			if p.Z < minZFirst {
				minZFirst = p.Z
			}
		}
	}
	if minZFirst < maxTopLast-1e-9 {
		t.Errorf("first stop's material (z≥%.1f) must sit on top of the last stop's tier (tops out %.1f)", minZFirst, maxTopLast)
	}

	// Steps are a 1..N permutation in pack order.
	for i, p := range plan.Placements {
		if p.Step != i+1 {
			t.Fatalf("placement %d has step %d; want %d", i, p.Step, i+1)
		}
	}

	if plan.MaxLoadHeightIn <= 0 {
		t.Errorf("expected a positive max load height, got %v", plan.MaxLoadHeightIn)
	}
	if plan.GVWStatus == "" {
		t.Error("expected a GVW status")
	}
}

func TestSolveSequencedBundlesRespectsBedEnvelope(t *testing.T) {
	v := sequencedTestVehicle()
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x4", Quantity: 400, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true},
			{ProductID: "p2", SKU: "NO-GEOM", Quantity: 5, WeightLbs: 10},
		}},
	}
	plan := SolveSequencedBundles(v, stops)

	for _, p := range plan.Placements {
		if p.X+p.LengthIn > v.BedLengthIn+1e-9 || p.Y+p.WidthIn > v.BedWidthIn+1e-9 || p.Z+p.HeightIn > v.BedHeightIn+1e-9 {
			t.Fatalf("placement %s exceeds bed envelope: x=%v y=%v z=%v", p.SKU, p.X, p.Y, p.Z)
		}
	}
	if len(plan.Unplaced) == 0 {
		t.Error("expected the no-geometry item to be reported unplaced")
	}
}

func TestBundleShapeSquareish(t *testing.T) {
	// Stackable: only a stackable article may be banded more than one high.
	it := Item{LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, Stackable: true}
	cols, layers := bundleShape(96, it, sequencedTestVehicle(), 96)
	if cols < 2 || layers < 2 {
		t.Errorf("expected a stacked bundle cross-section, got %d cols × %d layers", cols, layers)
	}
	if float64(layers)*it.HeightIn > 30+1e-9 {
		t.Errorf("bundle %d layers exceeds the 30in band cap", layers)
	}
}

// TestBundleShapeNonStackableIsSingleLayer verifies bundleShape never bands a
// non-stackable article more than one unit high however much headroom remains —
// it widens the bundle across the bed instead.
func TestBundleShapeNonStackableIsSingleLayer(t *testing.T) {
	v := sequencedTestVehicle()
	it := Item{LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, Stackable: false}
	cols, layers := bundleShape(96, it, v, 96)
	if layers != 1 {
		t.Errorf("non-stackable article banded %d layers high; want exactly 1", layers)
	}
	if cols < 2 {
		t.Errorf("expected the height-capped bundle to widen across the bed, got %d cols", cols)
	}
	if float64(cols)*it.WidthIn > v.BedWidthIn+1e-9 {
		t.Errorf("bundle of %d cols (%.1f in) is wider than the %.1f in bed", cols, float64(cols)*it.WidthIn, v.BedWidthIn)
	}
}

// overlapsXY reports whether two placements share bed footprint.
func overlapsXY(a, b Placement) bool {
	const eps = 1e-9
	return a.X < b.X+b.LengthIn-eps && b.X < a.X+a.LengthIn-eps &&
		a.Y < b.Y+b.WidthIn-eps && b.Y < a.Y+a.WidthIn-eps
}

// assertNoOverlayOnNonStackable fails if any placement sits above a placement of
// a non-stackable SKU whose footprint it shares. This is the physical-safety
// invariant: the yard's pack manifest must never say "put this on the slab".
func assertNoOverlayOnNonStackable(t *testing.T, plan Plan, nonStackableSKUs ...string) {
	t.Helper()
	blocked := make(map[string]bool, len(nonStackableSKUs))
	for _, sku := range nonStackableSKUs {
		blocked[sku] = true
	}
	for i, base := range plan.Placements {
		if !blocked[base.SKU] {
			continue
		}
		top := base.Z + base.HeightIn
		for j, over := range plan.Placements {
			if i == j || over.Z < top-1e-9 {
				continue
			}
			if overlapsXY(base, over) {
				t.Errorf("%s (%s, z=%.1f–%.1f) is overlaid by %s (%s, z=%.1f): nothing may be stacked on a non-stackable article",
					base.SKU, base.OrderID, base.Z, top, over.SKU, over.OrderID, over.Z)
			}
		}
	}
}

// TestNonStackableSkuIsNeverMultiLayered verifies the production tier-packer
// honours Stackable: a non-stackable SKU is laid one unit high across the bed,
// never banded into multi-layer bundles.
func TestNonStackableSkuIsNeverMultiLayered(t *testing.T) {
	v := sequencedTestVehicle()
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "slab", SKU: "STONE-SLAB", Quantity: 6, LengthIn: 72, WidthIn: 36, HeightIn: 3, WeightLbs: 400, Stackable: false},
		}},
	}
	plan := SolveSequencedBundles(v, stops)

	if len(plan.Placements) != 6 {
		t.Fatalf("expected all 6 slabs placed, got %d (unplaced: %v)", len(plan.Placements), plan.Unplaced)
	}
	for _, p := range plan.Placements {
		if p.Z != 0 {
			t.Errorf("non-stackable slab placed at z=%.1f; a non-stackable article must stay on the deck", p.Z)
		}
	}
	if plan.MaxLoadHeightIn > 3+1e-9 {
		t.Errorf("load is %.1f in tall — a single layer of 3 in slabs must not exceed 3 in", plan.MaxLoadHeightIn)
	}
	assertNoOverlayOnNonStackable(t, plan, "STONE-SLAB")
}

// TestNonStackableTierIsNeverOverlaid verifies no later tier is piled on top of
// a stop whose material is non-stackable: the load is sealed and the material
// that will not fit is reported unplaced instead of silently crushing the slab.
func TestNonStackableTierIsNeverOverlaid(t *testing.T) {
	v := sequencedTestVehicle()
	slab := Item{ProductID: "slab", SKU: "STONE-SLAB", Quantity: 4, LengthIn: 72, WidthIn: 36, HeightIn: 3, WeightLbs: 400, Stackable: false}
	lumber := Item{ProductID: "p1", SKU: "2x4", Quantity: 200, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true}

	// Stop 2 delivers last, so its tier is packed on the bottom; stop 1's
	// material would otherwise be tiered on top of it.
	stops := []StopItems{
		{OrderID: "first-stop", StopSequence: 1, Items: []Item{lumber}},
		{OrderID: "last-stop", StopSequence: 2, Items: []Item{slab}},
	}
	plan := SolveSequencedBundles(v, stops)

	assertNoOverlayOnNonStackable(t, plan, "STONE-SLAB")
	for _, p := range plan.Placements {
		if p.OrderID == "first-stop" {
			t.Fatalf("first-stop material was tiered on top of the non-stackable slab tier at z=%.1f", p.Z)
		}
	}
	sealed := false
	for _, u := range plan.Unplaced {
		if strings.Contains(u, "cannot stack on non-stackable") {
			sealed = true
		}
	}
	if !sealed {
		t.Errorf("expected the blocked material to be reported unplaced with a stackability reason, got %v", plan.Unplaced)
	}

	// Control: the ONLY thing keeping that tier off the truck is stackability —
	// with a stackable slab the same load tiers normally.
	slab.Stackable = true
	stops[1].Items = []Item{slab}
	ctl := SolveSequencedBundles(v, stops)
	placedFirst := false
	for _, p := range ctl.Placements {
		if p.OrderID == "first-stop" {
			placedFirst = true
		}
	}
	if !placedFirst {
		t.Error("control: with a stackable bottom tier the first stop's material should still be packed on top")
	}
}

// TestNonStackableSealsTheLevelWithinATier verifies the packer will not raise a
// level over a non-stackable article inside a single stop's tier either: the
// overflow is reported unplaced rather than stacked on the slab.
func TestNonStackableSealsTheLevelWithinATier(t *testing.T) {
	v := sequencedTestVehicle()
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "slab", SKU: "STONE-SLAB", Quantity: 2, LengthIn: 72, WidthIn: 36, HeightIn: 3, WeightLbs: 400, Stackable: false},
			{ProductID: "p1", SKU: "2x4", Quantity: 2000, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true},
		}},
	}
	plan := SolveSequencedBundles(v, stops)

	assertNoOverlayOnNonStackable(t, plan, "STONE-SLAB")

	var lumberPlaced, slabPlaced int
	for _, p := range plan.Placements {
		switch p.SKU {
		case "2x4":
			lumberPlaced++
		case "STONE-SLAB":
			slabPlaced++
			if p.Z != 0 {
				t.Errorf("slab placed at z=%.1f; it must stay on the deck", p.Z)
			}
		}
	}
	if slabPlaced != 2 {
		t.Errorf("expected both slabs placed on the deck, got %d (unplaced %v)", slabPlaced, plan.Unplaced)
	}
	if lumberPlaced == 0 {
		t.Error("lumber should still pack beside the slab on the same level")
	}
	blocked := false
	for _, u := range plan.Unplaced {
		if strings.Contains(u, "cannot stack on non-stackable") {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("expected the level rise over the slab to be refused and reported, got %v", plan.Unplaced)
	}
}

func TestSecurementPlan(t *testing.T) {
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x6x16", Quantity: 60, LengthIn: 192, WidthIn: 5.5, HeightIn: 1.5, WeightLbs: 27, Stackable: true},
		}},
	}
	plan := SolveSequencedBundles(sequencedTestVehicle(), stops)
	s := plan.Securement
	if s == nil {
		t.Fatal("expected a securement plan")
	}
	// 16 ft article → 2 for first 10 ft + 1 for the fraction = 3 straps.
	if len(s.Straps) < 3 {
		t.Errorf("16 ft load needs ≥3 tie-downs, got %d", len(s.Straps))
	}
	if s.MinAggregateWLLLbs < int64(0.5*60*27) {
		t.Errorf("aggregate WLL %d below 50%% of cargo", s.MinAggregateWLLLbs)
	}
	var sum int64
	for _, st := range s.Straps {
		sum += st.RequiredWLLLbs
		if st.OverHeightIn <= 0 {
			t.Errorf("strap %d has no over-height", st.Number)
		}
	}
	if sum < s.MinAggregateWLLLbs {
		t.Errorf("strap WLL shares (%d) do not cover the aggregate requirement (%d)", sum, s.MinAggregateWLLLbs)
	}
}
