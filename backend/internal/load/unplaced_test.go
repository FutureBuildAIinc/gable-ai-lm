// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"encoding/json"
	"testing"
)

// TestPackerTypesNoGeometrySeparatelyFromDidNotFit is the regression guard for
// the defect that made a joist hanger undispatchable: the packer reported "no
// geometry" and "truck full" as indistinguishable strings, so every consumer
// had to treat "there is nothing to pack here" as "this would not fit".
//
// One solve, both kinds of entry, and they must be told apart structurally —
// not by reading the prose.
func TestPackerTypesNoGeometrySeparatelyFromDidNotFit(t *testing.T) {
	v := sequencedTestVehicle()
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			// Dimensionless: a box of joist hangers. Nothing to place.
			{ProductID: "hgr", SKU: "HANGER-26", Quantity: 120, WeightLbs: 0.75, Stackable: true},
			// Genuinely more lumber than the deck can take.
			{ProductID: "p1", SKU: "2x4", Quantity: 20000, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true},
		}},
	}
	plan := SolveSequencedBundles(v, stops)

	var hanger, lumber *Unplaced
	for i := range plan.Unplaced {
		switch plan.Unplaced[i].SKU {
		case "HANGER-26":
			hanger = &plan.Unplaced[i]
		case "2x4":
			lumber = &plan.Unplaced[i]
		}
	}
	if hanger == nil {
		t.Fatalf("the dimensionless SKU must still be reported, got %v", plan.Unplaced)
	}
	if lumber == nil {
		t.Fatalf("the overflowing lumber must be reported, got %v", plan.Unplaced)
	}

	if hanger.Reason != ReasonNoGeometry {
		t.Errorf("dimensionless SKU reason = %q, want %q", hanger.Reason, ReasonNoGeometry)
	}
	if hanger.Blocking() {
		t.Error("a SKU with no geometry to place must NOT block a dispatch — nothing about it says it would not fit")
	}
	if !hanger.Rides() {
		t.Error("a SKU with no geometry still travels on the truck; it is only absent from the 3D plan")
	}
	if hanger.Quantity != 120 {
		t.Errorf("dimensionless SKU quantity = %d, want 120", hanger.Quantity)
	}

	if lumber.Reason == ReasonNoGeometry || !lumber.Blocking() {
		t.Errorf("cargo that did not fit must block: %+v", *lumber)
	}
	if lumber.Rides() {
		t.Error("cargo that did not fit stays in the yard — it must not read as riding")
	}

	blocking, noGeom := SplitUnplaced(plan.Unplaced)
	if len(noGeom) != 1 || noGeom[0].SKU != "HANGER-26" {
		t.Errorf("SplitUnplaced no-geometry side = %v, want just HANGER-26", noGeom)
	}
	if len(blocking) == 0 {
		t.Error("SplitUnplaced blocking side must carry the dropped lumber")
	}
}

// TestUnmodeledCargoStaysInTheExactGross pins the consequence of letting a
// no-geometry load dispatch: the article rides, so its weight must remain in the
// gross the GVW gate is computed from. Losing it there would trade a false
// refusal for a silently-light truck, which is the worse failure.
func TestUnmodeledCargoStaysInTheExactGross(t *testing.T) {
	v := sequencedTestVehicle()
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "hgr", SKU: "HANGER-26", Quantity: 120, WeightLbs: 5},
		}},
	}
	plan := SolveSequencedBundles(v, stops)

	if len(plan.Placements) != 0 {
		t.Fatalf("a dimensionless SKU must not be placed as a zero-size box, got %d placements", len(plan.Placements))
	}
	if plan.UnmodeledWeightLbs != 600 {
		t.Errorf("unmodeled weight = %d lb, want 600 (120 × 5)", plan.UnmodeledWeightLbs)
	}
	want := v.TareWeightLbs + 600
	if plan.TotalWeightLbs != want {
		t.Errorf("gross = %d lb, want %d — cargo that rides must stay in the exact gross even when it cannot be placed",
			plan.TotalWeightLbs, want)
	}

	// Control: cargo that did NOT FIT is not on the truck, so it must not be
	// added to the gross.
	full := SolveSequencedBundles(v, []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x4", Quantity: 20000, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true},
		}},
	})
	if full.UnmodeledWeightLbs != 0 {
		t.Errorf("dropped cargo must not count as riding weight, got %d lb", full.UnmodeledWeightLbs)
	}
}

// TestUnplacedRoundTripsAndAcceptsLegacyStrings covers persistence: plans live
// in JSONB, so a run written before the reason code existed must still load —
// and must be classified rather than defaulting to the benign side.
func TestUnplacedRoundTripsAndAcceptsLegacyStrings(t *testing.T) {
	t.Run("typed round-trip", func(t *testing.T) {
		in := []Unplaced{{SKU: "HANGER-26", Quantity: 120, Reason: ReasonNoGeometry, Detail: "no geometry recorded", WeightLbs: 90}}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out []Unplaced
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(out) != 1 || out[0] != in[0] {
			t.Fatalf("round-trip = %+v, want %+v", out, in)
		}
	})

	cases := []struct {
		legacy   string
		wantSKU  string
		wantQty  int
		wantCode string
		blocking bool
	}{
		{`"HANGER-26 ×120 (no geometry)"`, "HANGER-26", 120, ReasonNoGeometry, false},
		{`"2x4 ×48 (truck full)"`, "2x4", 48, ReasonTruckFull, true},
		{`"STEP-SLAB ×2 (bed volume full)"`, "STEP-SLAB", 2, ReasonVolumeFull, true},
		{`"LVL-24 ×3 (too large for bed)"`, "LVL-24", 3, ReasonTooLarge, true},
		{`"MLD-BASE ×40 (cannot stack on non-stackable DR-EXT-3680-STL)"`, "MLD-BASE", 40, ReasonNotStackable, true},
		// Unrecognised prose must fall to the blocking side, not the benign one.
		{`"MYSTERY ×1 (something new)"`, "MYSTERY", 1, ReasonTruckFull, true},
	}
	for _, tc := range cases {
		t.Run(tc.legacy, func(t *testing.T) {
			var u Unplaced
			if err := json.Unmarshal([]byte(tc.legacy), &u); err != nil {
				t.Fatalf("a legacy string entry must still load: %v", err)
			}
			if u.SKU != tc.wantSKU || u.Quantity != tc.wantQty {
				t.Errorf("parsed %+v, want sku=%q qty=%d", u, tc.wantSKU, tc.wantQty)
			}
			if u.Reason != tc.wantCode {
				t.Errorf("reason = %q, want %q", u.Reason, tc.wantCode)
			}
			if u.Blocking() != tc.blocking {
				t.Errorf("blocking = %v, want %v", u.Blocking(), tc.blocking)
			}
		})
	}
}

// TestUnknownReasonBlocks pins the fail-closed default directly: a reason a
// future packer adds must refuse a dispatch until somebody classifies it.
func TestUnknownReasonBlocks(t *testing.T) {
	for _, reason := range []string{"", "SOMETHING_NEW", "no_geometry", "NO-GEOMETRY"} {
		u := Unplaced{SKU: "X", Quantity: 1, Reason: reason}
		if !u.Blocking() {
			t.Errorf("reason %q must block — only the exact %s code is benign", reason, ReasonNoGeometry)
		}
	}
}
