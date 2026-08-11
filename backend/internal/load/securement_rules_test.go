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
