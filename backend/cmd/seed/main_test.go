// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package main

import (
	"os"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
)

// TestDemoSeedRequested pins the opt-in gate. Anything that is not clearly a
// yes must be a no: the failure mode of getting this wrong is a real dealer's
// database acquiring a fictional dealer's fleet.
func TestDemoSeedRequested(t *testing.T) {
	cases := []struct {
		value string
		set   bool
		want  bool
	}{
		{set: false, want: false},
		{value: "", set: true, want: false},
		{value: "0", set: true, want: false},
		{value: "false", set: true, want: false},
		{value: "no", set: true, want: false},
		{value: "off", set: true, want: false},
		{value: "please", set: true, want: false},
		{value: "1", set: true, want: true},
		{value: "true", set: true, want: true},
		{value: "TRUE", set: true, want: true},
		{value: "  yes  ", set: true, want: true},
		{value: "on", set: true, want: true},
	}
	for _, tc := range cases {
		name := "unset"
		if tc.set {
			name = "value=" + tc.value
		}
		t.Run(name, func(t *testing.T) {
			// An empty value is distinct from unset, so cover both. t.Setenv
			// first so its cleanup restores whatever the caller's env had.
			t.Setenv(demoSeedEnv, tc.value)
			if !tc.set {
				if err := os.Unsetenv(demoSeedEnv); err != nil {
					t.Fatalf("unset %s: %v", demoSeedEnv, err)
				}
			}
			if got := demoSeedRequested(); got != tc.want {
				t.Fatalf("demoSeedRequested() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNaturalKey covers the matching rule that turns three unconditional
// INSERTs into an upsert.
func TestNaturalKey(t *testing.T) {
	same := []string{
		"Bennett Bridge (W.R. Bennett)",
		"bennett bridge (w.r. bennett)",
		"  Bennett   Bridge  (W.R. Bennett)  ",
		"BENNETT\tBRIDGE (W.R. BENNETT)",
	}
	want := naturalKey(same[0])
	for _, s := range same[1:] {
		if got := naturalKey(s); got != want {
			t.Errorf("naturalKey(%q) = %q, want %q", s, got, want)
		}
	}
	if naturalKey("McCulloch Rd Culvert") == want {
		t.Error("distinct names must not collide")
	}
}

func storedPoint(mut func(*compliance.RestrictedPoint)) compliance.RestrictedPoint {
	p := compliance.RestrictedPoint{
		ID:                "rp-1",
		Name:              "Bennett Bridge (W.R. Bennett)",
		Lat:               49.8845,
		Lng:               -119.4960,
		RestrictionType:   "WEIGHT",
		MaxGrossWeightLbs: i64(21000),
		Notes:             "Floating bridge — temporary gross-weight restriction during deck repair.",
	}
	if mut != nil {
		mut(&p)
	}
	return p
}

// TestPointMatches decides whether a re-run rewrites a row or leaves it alone.
// A false "already current" would let drift persist; a false "changed" would
// churn updated_at on every deploy.
func TestPointMatches(t *testing.T) {
	want := demoPoints()[0] // Bennett Bridge

	if !pointMatches(storedPoint(nil), want) {
		t.Fatal("an identical stored point should be reported as current")
	}
	if !pointMatches(storedPoint(func(p *compliance.RestrictedPoint) { p.RestrictionType = "weight" }), want) {
		t.Error("restriction type should compare case-insensitively")
	}

	changed := map[string]func(*compliance.RestrictedPoint){
		"moved":               func(p *compliance.RestrictedPoint) { p.Lat = 49.9 },
		"moved east":          func(p *compliance.RestrictedPoint) { p.Lng = -119.0 },
		"limit raised":        func(p *compliance.RestrictedPoint) { p.MaxGrossWeightLbs = i64(30000) },
		"limit removed":       func(p *compliance.RestrictedPoint) { p.MaxGrossWeightLbs = nil },
		"other limit added":   func(p *compliance.RestrictedPoint) { p.MaxAxleWeightLbs = i64(18000) },
		"height limit added":  func(p *compliance.RestrictedPoint) { p.MaxHeightIn = f64(136) },
		"notes edited":        func(p *compliance.RestrictedPoint) { p.Notes = "reopened" },
		"restriction retyped": func(p *compliance.RestrictedPoint) { p.RestrictionType = "SEASONAL" },
	}
	for name, mut := range changed {
		if pointMatches(storedPoint(mut), want) {
			t.Errorf("%s: expected a difference to be detected", name)
		}
	}
}

// TestDemoPointsAreDistinct guards the seed's own data: two demo points that
// normalise to the same natural key would make the upsert non-convergent.
func TestDemoPointsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range demoPoints() {
		k := naturalKey(p.Name)
		if seen[k] {
			t.Fatalf("duplicate natural key in demoPoints: %q", k)
		}
		seen[k] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 demo points, got %d", len(seen))
	}
}

// TestCountForeign drives the "this is not a scratch database" warning.
func TestCountForeign(t *testing.T) {
	wanted := demoPoints()
	existing := []compliance.RestrictedPoint{
		{Name: "bennett  bridge (w.r. bennett)"}, // demo point, differently cased
		{Name: "Mission Creek Bridge"},           // the dealer's own
		{Name: "Glenmore Rd Weight Limit"},       // the dealer's own
	}
	if got := countForeign(existing, wanted); got != 2 {
		t.Fatalf("countForeign = %d, want 2", got)
	}
	if got := countForeign(nil, wanted); got != 0 {
		t.Fatalf("countForeign on an empty registry = %d, want 0", got)
	}
}

// TestDemoProfilesAreCompleteEnoughToBeSafe: the load solver treats a zero
// GVWR or a zero axle rating as "unrated" and returns PASS for any load, so a
// seed that shipped a blank rating would hand out confident wrong answers.
func TestDemoProfilesAreCompleteEnoughToBeSafe(t *testing.T) {
	for _, p := range demoProfiles() {
		if p.vehicleID == "" {
			t.Errorf("%s: missing vehicle id", p.input.Name)
		}
		if p.input.GVWRLbs <= 0 {
			t.Errorf("%s: GVWR must be positive, got %d", p.input.Name, p.input.GVWRLbs)
		}
		if p.input.BedLengthIn <= 0 || p.input.BedWidthIn <= 0 || p.input.BedHeightIn <= 0 {
			t.Errorf("%s: bed dimensions must be positive", p.input.Name)
		}
		if len(p.input.Axles) == 0 {
			t.Fatalf("%s: no axles", p.input.Name)
		}
		var rated int64
		for _, a := range p.input.Axles {
			if a.MaxWeightLbs <= 0 {
				t.Errorf("%s: axle %d has no rating", p.input.Name, a.AxleNumber)
			}
			rated += a.MaxWeightLbs
		}
		if rated < p.input.GVWRLbs {
			t.Errorf("%s: axle ratings sum to %d, below GVWR %d", p.input.Name, rated, p.input.GVWRLbs)
		}
		if p.input.TareWeightLbs >= p.input.GVWRLbs {
			t.Errorf("%s: tare %d leaves no payload under GVWR %d",
				p.input.Name, p.input.TareWeightLbs, p.input.GVWRLbs)
		}
	}
}
