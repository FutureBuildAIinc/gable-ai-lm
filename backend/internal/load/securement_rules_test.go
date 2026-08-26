// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"math"
	"testing"
)

func TestResolveSecurementRuleset(t *testing.T) {
	if rs := resolveSecurementRuleset(""); rs.Code != "US_FMCSA" {
		t.Errorf("blank jurisdiction should default to US_FMCSA, got %s", rs.Code)
	}
	if rs := resolveSecurementRuleset("bogus"); rs.Code != "US_FMCSA" {
		t.Errorf("unknown jurisdiction should default to US_FMCSA, got %s", rs.Code)
	}
	if rs := resolveSecurementRuleset("ca_nsc"); rs.Code != "CA_NSC" {
		t.Errorf("ca_nsc should resolve to CA_NSC, got %s", rs.Code)
	}
}

func TestRequiredTieDownsByLength(t *testing.T) {
	us := resolveSecurementRuleset("US_FMCSA")
	cases := []struct {
		spanFt float64
		want   int
	}{
		{5, 2},  // ≤ first segment → base
		{10, 2}, // exactly first segment → base
		{16, 3}, // +1 for the fraction over 10 ft
		{25, 4}, // 2 + ceil(15/10) = 4
	}
	for _, c := range cases {
		if got := us.requiredTieDowns(c.spanFt, 1000); got != c.want {
			t.Errorf("US tie-downs for %.0f ft = %d, want %d", c.spanFt, got, c.want)
		}
	}
}

// TestRequiredTieDownsWeightRule verifies the per-weight rule path: a stricter
// ruleset that mandates one tie-down per N lb escalates the count for a heavy
// short load beyond the length minimum (data-driven extensibility).
func TestRequiredTieDownsWeightRule(t *testing.T) {
	strict := SecurementRuleset{
		BaseTieDowns: 2, FirstSegmentFt: 10, AdditionalPerFt: 10,
		MaxWeightPerTieDownLbs: 5000,
	}
	// 8 ft article (length rule → 2) but 22,000 lb → ceil(22000/5000)=5.
	if got := strict.requiredTieDowns(8, 22000); got != 5 {
		t.Errorf("weight rule tie-downs = %d, want 5", got)
	}
}

// TestComputeSecurementSurfacesRuleBasisAndAnchors verifies the securement
// output records the jurisdiction rule basis, snaps straps to the modeled anchor
// grid, and meets the aggregate WLL fraction.
func TestComputeSecurementSurfacesRuleBasisAndAnchors(t *testing.T) {
	v := sequencedTestVehicle()
	v.SecurementJurisdiction = "CA_NSC"
	v.AnchorSpacingIn = 24
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x6x16", Quantity: 60, LengthIn: 192, WidthIn: 5.5, HeightIn: 1.5, WeightLbs: 27, Stackable: true},
		}},
	}
	plan := SolveSequencedBundles(v, stops)
	s := plan.Securement
	if s == nil {
		t.Fatal("expected a securement plan")
	}
	if s.Jurisdiction != "CA_NSC" || s.RuleBasis == "" || s.RulesetName == "" {
		t.Errorf("rule basis not surfaced: jurisdiction=%q name=%q basis=%q", s.Jurisdiction, s.RulesetName, s.RuleBasis)
	}
	if s.RequiredTieDowns < 3 {
		t.Errorf("16 ft load needs ≥3 tie-downs by rule, got %d", s.RequiredTieDowns)
	}
	if s.AnchorSpacingIn != 24 {
		t.Errorf("anchor spacing not surfaced, got %.1f", s.AnchorSpacingIn)
	}
	// Every strap must land on a modeled anchor (a multiple of the spacing).
	for _, st := range s.Straps {
		if r := math.Mod(st.PositionIn, v.AnchorSpacingIn); math.Abs(r) > 1e-6 && math.Abs(r-v.AnchorSpacingIn) > 1e-6 {
			t.Errorf("strap %d at %.1f in is not on the %0.f in anchor grid", st.Number, st.PositionIn, v.AnchorSpacingIn)
		}
	}
	// Aggregate WLL must be ≥ the ruleset fraction of cargo weight.
	cargo := 60.0 * 27.0
	if float64(s.MinAggregateWLLLbs) < 0.5*cargo-1 {
		t.Errorf("aggregate WLL %d below 50%% of cargo %.0f", s.MinAggregateWLLLbs, cargo)
	}
}

// TestSecurementJurisdictionChangesBasis verifies switching jurisdiction changes
// the surfaced rule basis for the same load.
func TestSecurementJurisdictionChangesBasis(t *testing.T) {
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x6x16", Quantity: 60, LengthIn: 192, WidthIn: 5.5, HeightIn: 1.5, WeightLbs: 27, Stackable: true},
		}},
	}
	us := sequencedTestVehicle()
	us.SecurementJurisdiction = "US_FMCSA"
	ca := sequencedTestVehicle()
	ca.SecurementJurisdiction = "CA_NSC"

	usPlan := SolveSequencedBundles(us, stops)
	caPlan := SolveSequencedBundles(ca, stops)
	if usPlan.Securement.RuleBasis == caPlan.Securement.RuleBasis {
		t.Error("expected different rule basis text between US and CA jurisdictions")
	}
}

// TestSecurementMeetsItsOwnRequirements is the invariant nobody had asserted:
// the strap LIST a securement plan publishes must satisfy the requirements the
// SAME plan states next to it. Two things have to hold on every load:
//
//   - at least RequiredTieDowns straps — the ruleset minimum is a legal count,
//     not a target;
//   - the sum of every strap's RequiredWLLLbs ≥ MinAggregateWLLLbs — the whole
//     point of the aggregate rule.
//
// Both were violated whenever the load span carried fewer modeled bed anchors
// than the rule demanded straps: anchorPositions returned only the anchors it
// found, while the per-strap WLL share had already been divided by the count it
// never emitted. Two 40 in pallets of block at 5,000 lb each came back with ONE
// strap rated 2,500 lb against a stated 5,000 lb aggregate minimum — a yard
// following the manifest exactly would have under-secured 10,000 lb by half.
//
// The short-span cases are listed first because they are the ones that failed;
// the long lumber cases are the control, and their straps must still land on
// the anchor grid.
func TestSecurementMeetsItsOwnRequirements(t *testing.T) {
	cases := []struct {
		name        string
		spacingIn   float64
		item        Item
		wantSnapped bool // every strap on a modeled anchor
	}{
		{
			// The reported defect: one row of pallets, span 40 in, one anchor.
			name:      "short heavy pallet run on a coarse anchor pitch",
			spacingIn: 24,
			item:      Item{ProductID: "p1", SKU: "BLOCK-PALLET", Quantity: 2, LengthIn: 40, WidthIn: 48, HeightIn: 40, WeightLbs: 5000},
		},
		{
			// Span shorter than a single anchor cell.
			name:      "single crate narrower than one anchor cell",
			spacingIn: 48,
			item:      Item{ProductID: "p2", SKU: "WINDOW-CRATE", Quantity: 1, LengthIn: 30, WidthIn: 40, HeightIn: 60, WeightLbs: 900},
		},
		{
			// Heavy enough that the 4" winch escalation raises the count well
			// past the anchors the span contains.
			name:      "heavy stone slabs escalating the strap count past the anchors",
			spacingIn: 36,
			item:      Item{ProductID: "p3", SKU: "STONE-SLAB", Quantity: 2, LengthIn: 48, WidthIn: 48, HeightIn: 6, WeightLbs: 9000},
		},
		{
			name:        "16 ft lumber bundle (control — plenty of anchors)",
			spacingIn:   24,
			item:        Item{ProductID: "p4", SKU: "2x6x16", Quantity: 60, LengthIn: 192, WidthIn: 5.5, HeightIn: 1.5, WeightLbs: 27, Stackable: true},
			wantSnapped: true,
		},
	}

	for _, jurisdiction := range []string{"US_FMCSA", "CA_NSC"} {
		for _, tc := range cases {
			t.Run(jurisdiction+"/"+tc.name, func(t *testing.T) {
				v := sequencedTestVehicle()
				v.SecurementJurisdiction = jurisdiction
				v.AnchorSpacingIn = tc.spacingIn

				plan := SolveSequencedBundles(v, []StopItems{
					{OrderID: "o1", StopSequence: 1, Items: []Item{tc.item}},
				})
				if len(plan.Placements) == 0 {
					t.Fatalf("nothing packed, so there is no securement to judge: %v", plan.Unplaced)
				}
				s := plan.Securement
				if s == nil {
					t.Fatal("a packed load must carry a securement plan")
				}

				if len(s.Straps) < s.RequiredTieDowns {
					t.Errorf("plan states %d tie-downs are required but publishes only %d straps",
						s.RequiredTieDowns, len(s.Straps))
				}

				var delivered int64
				for _, st := range s.Straps {
					delivered += st.RequiredWLLLbs
					if st.RequiredWLLLbs > wll4InWinchLbs {
						t.Errorf("strap %d needs WLL %d lb, above the heaviest strap this engine recommends (%d lb)",
							st.Number, st.RequiredWLLLbs, wll4InWinchLbs)
					}
				}
				if delivered < s.MinAggregateWLLLbs {
					t.Errorf("the %d recommended straps total %d lb of WLL, below this plan's own %d lb aggregate minimum",
						len(s.Straps), delivered, s.MinAggregateWLLLbs)
				}

				// Straps must cross the load, wherever they were placed.
				minX, maxX := math.Inf(1), math.Inf(-1)
				for _, p := range plan.Placements {
					if p.X < minX {
						minX = p.X
					}
					if end := p.X + p.LengthIn; end > maxX {
						maxX = end
					}
				}
				for _, st := range s.Straps {
					if st.PositionIn < minX-0.05 || st.PositionIn > maxX+0.05 {
						t.Errorf("strap %d at %.1f in is outside the load span %.1f..%.1f in and holds nothing down",
							st.Number, st.PositionIn, minX, maxX)
					}
				}

				if tc.wantSnapped {
					for _, st := range s.Straps {
						if r := math.Mod(st.PositionIn, tc.spacingIn); math.Abs(r) > 1e-6 && math.Abs(r-tc.spacingIn) > 1e-6 {
							t.Errorf("strap %d at %.1f in is not on the %.0f in anchor grid", st.Number, st.PositionIn, tc.spacingIn)
						}
					}
				}
			})
		}
	}
}

// TestAnchorPositionsAlwaysReturnsTheRequestedCount pins the contract
// computeSecurement now divides the aggregate WLL by: n straps in, n positions
// out. It previously returned only the anchors it happened to find.
func TestAnchorPositionsAlwaysReturnsTheRequestedCount(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 9} {
		for _, span := range [][2]float64{{124, 164}, {0, 288}, {10, 11}, {48, 240}} {
			got, snapped := anchorPositions(span[0], span[1], 24, n)
			if len(got) != n {
				t.Errorf("anchorPositions(%.0f..%.0f, n=%d) returned %d positions, want %d",
					span[0], span[1], n, len(got), n)
			}
			for i := 1; i < len(got); i++ {
				if got[i] < got[i-1] {
					t.Errorf("anchorPositions(%.0f..%.0f, n=%d) is not sorted: %v", span[0], span[1], n, got)
					break
				}
			}
			if snapped {
				for _, p := range got {
					if r := math.Mod(p, 24); math.Abs(r) > 1e-6 && math.Abs(r-24) > 1e-6 {
						t.Errorf("claimed snapped, but %.1f is not on the 24 in grid (%v)", p, got)
					}
				}
			}
		}
	}
}
