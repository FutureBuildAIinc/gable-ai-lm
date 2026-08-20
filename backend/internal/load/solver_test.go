// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"math"
	"strings"
	"testing"
)

func testVehicle() Vehicle {
	return Vehicle{
		GableVehicleID: "veh-1",
		BedLengthIn:    240,
		BedWidthIn:     96,
		BedHeightIn:    96,
		GVWRLbs:        26000,
		TareWeightLbs:  12000,
		Axles: []Axle{
			{AxleNumber: 1, MaxWeightLbs: 12000, PositionFromFrontIn: 0, AxleType: "STEER"},
			{AxleNumber: 2, MaxWeightLbs: 20000, PositionFromFrontIn: 200, AxleType: "DRIVE"},
		},
	}
}

func TestSolvePlacesItemsWithinBed(t *testing.T) {
	s := NewShelfSolver()
	items := []Item{
		{ProductID: "p1", SKU: "2x4", Quantity: 4, LengthIn: 96, WidthIn: 24, HeightIn: 12, WeightLbs: 100, Stackable: true},
	}
	plan := s.Solve(testVehicle(), items)

	if len(plan.Placements) != 4 {
		t.Fatalf("expected 4 placements, got %d", len(plan.Placements))
	}
	if len(plan.Unplaced) != 0 {
		t.Fatalf("expected nothing unplaced, got %v", plan.Unplaced)
	}
	for _, p := range plan.Placements {
		if p.X < 0 || p.X+p.LengthIn > testVehicle().BedLengthIn {
			t.Errorf("placement out of bed length: x=%.1f len=%.1f", p.X, p.LengthIn)
		}
		if p.Y < 0 || p.Y+p.WidthIn > testVehicle().BedWidthIn {
			t.Errorf("placement out of bed width: y=%.1f w=%.1f", p.Y, p.WidthIn)
		}
	}
}

func TestSolveTotalWeightIncludesTare(t *testing.T) {
	s := NewShelfSolver()
	items := []Item{
		{ProductID: "p1", SKU: "block", Quantity: 2, LengthIn: 48, WidthIn: 48, HeightIn: 12, WeightLbs: 500, Stackable: false},
	}
	plan := s.Solve(testVehicle(), items)
	want := int64(12000 + 1000) // tare + 2*500
	if plan.TotalWeightLbs != want {
		t.Fatalf("total weight = %d, want %d", plan.TotalWeightLbs, want)
	}
}

func TestSolveFlagsOverweightGVW(t *testing.T) {
	s := NewShelfSolver()
	// 30 boxes * 1000 lbs = 30000 + 12000 tare = 42000 > 26000 GVWR.
	items := []Item{
		{ProductID: "p1", SKU: "steel", Quantity: 30, LengthIn: 24, WidthIn: 24, HeightIn: 12, WeightLbs: 1000, Stackable: true},
	}
	plan := s.Solve(testVehicle(), items)
	if plan.GVWStatus != "FAIL" {
		t.Fatalf("expected GVW FAIL for overweight load, got %s", plan.GVWStatus)
	}
}

// TestShelfSolverDoesNotRaiseOverNonStackableRow verifies the single-shot
// Solver surface (POST /api/v1/load/optimize) honours Item.Stackable in BOTH
// directions: material that will not fit beside a non-stackable article is
// reported unplaced rather than raised on top of it.
//
// The bed is shortened to one article-length row so the packer's only remaining
// move is upward — which is exactly what the stackability seal must refuse.
func TestShelfSolverDoesNotRaiseOverNonStackableRow(t *testing.T) {
	v := testVehicle()
	v.BedLengthIn = 48 // one row deep: the only way to fit more is to stack

	items := []Item{
		{ProductID: "slab", SKU: "STONE-SLAB", Quantity: 2, LengthIn: 48, WidthIn: 48, HeightIn: 12, WeightLbs: 500, Stackable: false},
		{ProductID: "p1", SKU: "2x4", Quantity: 2, LengthIn: 48, WidthIn: 48, HeightIn: 12, WeightLbs: 100, Stackable: true},
	}
	plan := NewShelfSolver().Solve(v, items)

	for _, p := range plan.Placements {
		if p.Z != 0 {
			t.Errorf("%s raised to z=%.1f over a deck sealed by a non-stackable slab", p.SKU, p.Z)
		}
	}
	assertNoOverlayOnNonStackable(t, plan, "STONE-SLAB")
	sealed := false
	for _, u := range plan.Unplaced {
		if strings.Contains(u, "cannot stack on non-stackable") {
			sealed = true
		}
	}
	if !sealed {
		t.Errorf("expected the blocked lumber to be reported unplaced with a stackability reason, got %v", plan.Unplaced)
	}

	// Control: the ONLY thing keeping that lumber off the truck is stackability,
	// so the guard is about stackability and not about disabling stacking
	// outright. With a stackable base the same load packs in full, on a real
	// second layer.
	items[0].Stackable = true
	ctl := NewShelfSolver().Solve(v, items)
	if len(ctl.Placements) != 4 {
		t.Fatalf("control: expected all 4 units packed on a stackable base, got %d (unplaced %v)",
			len(ctl.Placements), ctl.Unplaced)
	}
	raised := false
	for _, p := range ctl.Placements {
		if p.Z > 0 {
			raised = true
		}
	}
	if !raised {
		t.Error("control: a stackable base row should still support a second layer")
	}
}

// TestShelfSolverSecondLayerIsSupported asserts the physical invariant that a
// unit raised off the deck rests on something: its footprint must overlap a unit
// below it.
func TestShelfSolverSecondLayerIsSupported(t *testing.T) {
	v := testVehicle()
	items := []Item{
		{ProductID: "p1", SKU: "2x4", Quantity: 2, LengthIn: 48, WidthIn: 48, HeightIn: 12, WeightLbs: 100, Stackable: true},
	}
	plan := NewShelfSolver().Solve(v, items)

	// The two units must genuinely be banded one on top of the other, not laid
	// side by side — otherwise the support check below is vacuous.
	raised := 0
	for _, p := range plan.Placements {
		if p.Z > 0 {
			raised++
		}
	}
	if raised == 0 {
		t.Fatalf("expected the second stackable unit to be banded on top of the first, got %+v", plan.Placements)
	}

	for i, p := range plan.Placements {
		if p.Z == 0 {
			continue
		}
		supported := false
		for j, below := range plan.Placements {
			if i == j || below.Z+below.HeightIn > p.Z+1e-9 {
				continue
			}
			if overlapsXY(below, p) {
				supported = true
			}
		}
		if !supported {
			t.Errorf("%s at (x=%.1f y=%.1f z=%.1f) is raised off the deck with nothing beneath it",
				p.SKU, p.X, p.Y, p.Z)
		}
	}
}

// ---------------------------------------------------------------------------
// Profile completeness: an unrated profile must never read as a confident PASS.
// ---------------------------------------------------------------------------

// TestCompleteProfilePassesAndIsClean is the control for the UNKNOWN tests
// below: a fully-rated profile carrying a light load still reports PASS with no
// profile issues, so the guards cannot be satisfied by failing everything.
func TestCompleteProfilePassesAndIsClean(t *testing.T) {
	items := []Item{
		{ProductID: "p1", SKU: "2x4", Quantity: 4, LengthIn: 96, WidthIn: 24, HeightIn: 12, WeightLbs: 100, Stackable: true},
	}
	plan := NewShelfSolver().Solve(testVehicle(), items)

	if plan.GVWStatus != StatusPass {
		t.Errorf("GVW status = %s, want PASS for a light load on a complete profile", plan.GVWStatus)
	}
	if plan.ProfileStatus != ProfileComplete {
		t.Errorf("profile status = %q, want %q", plan.ProfileStatus, ProfileComplete)
	}
	if len(plan.ProfileIssues) != 0 {
		t.Errorf("expected no profile issues, got %v", plan.ProfileIssues)
	}
	for _, a := range plan.AxleLoads {
		if a.Status != StatusPass {
			t.Errorf("axle %d status = %s, want PASS", a.AxleNumber, a.Status)
		}
	}
}

// TestZeroAxleRatingIsUnknownNeverPass verifies a blank axle rating yields
// UNKNOWN for that axle and a non-passing overall verdict, instead of a zero
// utilization silently reading as a confident PASS.
func TestZeroAxleRatingIsUnknownNeverPass(t *testing.T) {
	v := testVehicle()
	v.Axles[1].MaxWeightLbs = 0 // drive axle rating never filled in on the Fleet page
	items := []Item{
		{ProductID: "p1", SKU: "2x4", Quantity: 4, LengthIn: 96, WidthIn: 24, HeightIn: 12, WeightLbs: 100, Stackable: true},
	}
	plan := NewShelfSolver().Solve(v, items)

	var unrated *AxleLoad
	for i := range plan.AxleLoads {
		if plan.AxleLoads[i].AxleNumber == 2 {
			unrated = &plan.AxleLoads[i]
		}
	}
	if unrated == nil {
		t.Fatal("axle 2 missing from the plan")
	}
	if unrated.Status != StatusUnknown {
		t.Errorf("unrated axle status = %s, want %s", unrated.Status, StatusUnknown)
	}
	if plan.GVWStatus == StatusPass {
		t.Error("a load judged against a blank axle rating must never report GVW PASS")
	}
	if plan.ProfileStatus != ProfileIncomplete {
		t.Errorf("profile status = %q, want %q", plan.ProfileStatus, ProfileIncomplete)
	}
	if len(plan.ProfileIssues) == 0 {
		t.Fatal("expected the blank axle rating to be surfaced in ProfileIssues")
	}
	if !strings.Contains(strings.Join(plan.ProfileIssues, " "), "axle 2") {
		t.Errorf("profile issues should name the unrated axle, got %v", plan.ProfileIssues)
	}
	// An unratable axle must not be averaged in as 0 utilization, which would
	// report a bogus "perfectly balanced" load.
	if plan.BalanceScore == 1 {
		t.Error("balance score must not report a perfect 1.0 when an axle cannot be judged")
	}
}

// TestZeroGVWRIsUnknownNeverPass verifies a blank GVWR does not skip the gross
// check and silently pass — exercised through the production sequenced packer.
func TestZeroGVWRIsUnknownNeverPass(t *testing.T) {
	v := sequencedTestVehicle()
	v.GVWRLbs = 0 // column defaults to 0 and the upsert path does not validate
	stops := []StopItems{
		{OrderID: "o1", StopSequence: 1, Items: []Item{
			{ProductID: "p1", SKU: "2x4", Quantity: 20, LengthIn: 96, WidthIn: 3.5, HeightIn: 1.5, WeightLbs: 9, Stackable: true},
		}},
	}
	plan := SolveSequencedBundles(v, stops)

	if plan.GVWStatus == StatusPass {
		t.Error("a load judged against a blank GVWR must never report GVW PASS")
	}
	if plan.ProfileStatus != ProfileIncomplete {
		t.Errorf("profile status = %q, want %q", plan.ProfileStatus, ProfileIncomplete)
	}
	if !strings.Contains(strings.Join(plan.ProfileIssues, " "), "GVWR") {
		t.Errorf("profile issues should name the missing GVWR, got %v", plan.ProfileIssues)
	}
}

// TestProfileWithNoAxlesIsUnknownNeverPass verifies an axle-less profile cannot
// report a compliant load just because there is nothing to exceed.
func TestProfileWithNoAxlesIsUnknownNeverPass(t *testing.T) {
	v := testVehicle()
	v.Axles = nil
	items := []Item{
		{ProductID: "p1", SKU: "2x4", Quantity: 4, LengthIn: 96, WidthIn: 24, HeightIn: 12, WeightLbs: 100, Stackable: true},
	}
	plan := NewShelfSolver().Solve(v, items)

	if plan.GVWStatus == StatusPass {
		t.Error("a profile with no axles must never report GVW PASS")
	}
	if plan.ProfileStatus != ProfileIncomplete {
		t.Errorf("profile status = %q, want %q", plan.ProfileStatus, ProfileIncomplete)
	}
	if !strings.Contains(strings.Join(plan.ProfileIssues, " "), "no axles") {
		t.Errorf("profile issues should name the missing axles, got %v", plan.ProfileIssues)
	}
}

// TestUtilStatusBoundaries pins the PASS/WARN/FAIL edges at util 0.90 and 1.00.
func TestUtilStatusBoundaries(t *testing.T) {
	cases := []struct {
		util float64
		want string
	}{
		{0, StatusPass},
		{0.8999, StatusPass},
		{0.90, StatusWarn},   // at the warn threshold, not below it
		{1.00, StatusWarn},   // exactly at rating is not yet a failure
		{1.0001, StatusFail}, // over rating
		{2.0, StatusFail},
	}
	for _, c := range cases {
		if got := utilStatus(c.util); got != c.want {
			t.Errorf("utilStatus(%.4f) = %s, want %s", c.util, got, c.want)
		}
	}
}

// TestRatedStatusBoundaries verifies the same edges against a real rating and
// that a missing rating is UNKNOWN rather than a zero-utilization PASS.
func TestRatedStatusBoundaries(t *testing.T) {
	const rating = 10000
	cases := []struct {
		load     float64
		rating   int64
		wantUtil float64
		want     string
	}{
		{5000, rating, 0.5, StatusPass},
		{8999, rating, 0.8999, StatusPass},
		{9000, rating, 0.90, StatusWarn},
		{10000, rating, 1.0, StatusWarn},
		{10001, rating, 1.0001, StatusFail},
		{5000, 0, 0, StatusUnknown},  // blank rating
		{5000, -1, 0, StatusUnknown}, // nonsense rating
		{0, 0, 0, StatusUnknown},     // empty truck, blank rating: still unknown
	}
	for _, c := range cases {
		util, st := ratedStatus(c.load, c.rating)
		if st != c.want {
			t.Errorf("ratedStatus(%.0f, %d) status = %s, want %s", c.load, c.rating, st, c.want)
		}
		if math.Abs(util-c.wantUtil) > 1e-9 {
			t.Errorf("ratedStatus(%.0f, %d) util = %v, want %v", c.load, c.rating, util, c.wantUtil)
		}
	}
}

// TestStatusRankUnknownIsNeverPass verifies UNKNOWN outranks every other verdict
// and collapses to a blocking FAIL on the published three-value GVW field.
func TestStatusRankUnknownIsNeverPass(t *testing.T) {
	if statusRank(StatusUnknown) <= statusRank(StatusFail) {
		t.Error("UNKNOWN must outrank FAIL so it dominates the roll-up")
	}
	if got := overallStatus(statusRank(StatusUnknown)); got == StatusPass {
		t.Errorf("overall status for UNKNOWN = %s; it must never be PASS", got)
	}
	for _, s := range []string{StatusPass, StatusWarn, StatusFail} {
		if got := overallStatus(statusRank(s)); got != s {
			t.Errorf("overallStatus(rank(%s)) = %s, want %s", s, got, s)
		}
	}
}

// ---------------------------------------------------------------------------
// Axle distribution: advisory labelling, position ordering, weight conservation.
// ---------------------------------------------------------------------------

// threeAxleTestVehicle has axle NUMBERS that are not position-monotonic: the tag
// axle (#3) physically sits between the steer (#1) and the drive (#2), which is
// how fleet profiles routinely number a tag/pusher configuration.
func threeAxleTestVehicle() Vehicle {
	return Vehicle{
		GableVehicleID: "veh-3axle",
		BedLengthIn:    288,
		BedWidthIn:     96,
		BedHeightIn:    96,
		GVWRLbs:        52000,
		TareWeightLbs:  0,
		Axles: []Axle{
			{AxleNumber: 1, MaxWeightLbs: 12000, PositionFromFrontIn: 0, AxleType: "STEER"},
			{AxleNumber: 2, MaxWeightLbs: 20000, PositionFromFrontIn: 240, AxleType: "DRIVE"},
			{AxleNumber: 3, MaxWeightLbs: 20000, PositionFromFrontIn: 120, AxleType: "TAG"},
		},
	}
}

func axleByNumber(t *testing.T, plan Plan, n int) AxleLoad {
	t.Helper()
	for _, a := range plan.AxleLoads {
		if a.AxleNumber == n {
			return a
		}
	}
	t.Fatalf("axle %d missing from plan (%+v)", n, plan.AxleLoads)
	return AxleLoad{}
}

// TestAxleDistributionUsesPositionOrderNotAxleNumber verifies weight is split
// between the axles that physically bracket the load, even when the profile
// delivers them in a non-position-monotonic axle_number order.
func TestAxleDistributionUsesPositionOrderNotAxleNumber(t *testing.T) {
	v := threeAxleTestVehicle()
	// One 1,000 lb unit centred at x = 60 in: exactly halfway between the steer
	// (0 in) and the tag (120 in), so each takes 500 lb and the drive takes none.
	plan := Plan{
		Placements: []Placement{{SKU: "beam", X: 40, LengthIn: 40, WeightLbs: 1000}},
		AxleLoads:  []AxleLoad{},
	}
	computeAxleLoads(&plan, v)

	if got := axleByNumber(t, plan, 1).WeightLbs; got != 500 {
		t.Errorf("steer axle = %d lb, want 500 (half the lever between 0 in and the tag at 120 in)", got)
	}
	if got := axleByNumber(t, plan, 3).WeightLbs; got != 500 {
		t.Errorf("tag axle = %d lb, want 500 — cargo must bracket against the physically adjacent axle", got)
	}
	if got := axleByNumber(t, plan, 2).WeightLbs; got != 0 {
		t.Errorf("drive axle at 240 in = %d lb, want 0 — it does not bracket cargo at 60 in", got)
	}
	// Results are still reported in the caller's axle_number order.
	for i, a := range plan.AxleLoads {
		if a.AxleNumber != v.Axles[i].AxleNumber {
			t.Errorf("axle load %d reported for axle %d, want %d (caller order must be preserved)",
				i, a.AxleNumber, v.Axles[i].AxleNumber)
		}
	}
}

// TestAxleCargoIsConserved verifies every pound of cargo lands on some axle —
// including cargo ahead of the first axle and behind the last, where the model
// assigns the whole weight to the end axle (see the package doc: overhang lever
// reactions are deliberately not modelled, which is why the split is advisory).
func TestAxleCargoIsConserved(t *testing.T) {
	v := threeAxleTestVehicle()
	v.Axles[0].PositionFromFrontIn = 36 // steer behind the bed nose: cargo can sit ahead of it
	v.TareWeightLbs = 14000
	plan := Plan{
		Placements: []Placement{
			{SKU: "nose", X: 0, LengthIn: 24, WeightLbs: 800},    // centre 12 in — ahead of every axle
			{SKU: "mid", X: 80, LengthIn: 40, WeightLbs: 1500},   // centre 100 in — between axles
			{SKU: "tail", X: 250, LengthIn: 36, WeightLbs: 1200}, // centre 268 in — behind every axle
		},
		AxleLoads: []AxleLoad{},
	}
	computeAxleLoads(&plan, v)

	var sum int64
	for _, a := range plan.AxleLoads {
		sum += a.WeightLbs
		if !a.Advisory {
			t.Errorf("axle %d is not marked advisory; per-axle numbers must never be presented as certified", a.AxleNumber)
		}
	}
	if plan.TotalWeightLbs != 800+1500+1200+14000 {
		t.Errorf("total weight = %d, want %d", plan.TotalWeightLbs, 800+1500+1200+14000)
	}
	if diff := sum - plan.TotalWeightLbs; diff > 2 || diff < -2 {
		t.Errorf("per-axle weights sum to %d but the load grosses %d — weight was lost or double-counted",
			sum, plan.TotalWeightLbs)
	}
	if len(plan.ProfileIssues) != 0 {
		t.Errorf("expected no profile issues on a fully rated profile, got %v", plan.ProfileIssues)
	}
}

// TestCheckCargoConservation verifies the guard that makes a lost or
// double-counted pound fail loudly rather than silently under-report an axle.
func TestCheckCargoConservation(t *testing.T) {
	if err := checkCargoConservation([]float64{400, 600}, 1000); err != nil {
		t.Errorf("exact split reported an error: %v", err)
	}
	if err := checkCargoConservation([]float64{400, 599.9}, 1000); err == nil {
		t.Error("a dropped 0.1 lb must be reported, not silently accepted")
	}
	if err := checkCargoConservation([]float64{400, 700}, 1000); err == nil {
		t.Error("double-counted weight must be reported")
	}
	if err := checkCargoConservation([]float64{}, 0); err != nil {
		t.Errorf("empty load reported an error: %v", err)
	}
}

func TestSolveIsDeterministic(t *testing.T) {
	s := NewShelfSolver()
	items := []Item{
		{ProductID: "a", SKU: "a", Quantity: 3, LengthIn: 40, WidthIn: 30, HeightIn: 20, WeightLbs: 200, Stackable: true},
		{ProductID: "b", SKU: "b", Quantity: 2, LengthIn: 50, WidthIn: 40, HeightIn: 20, WeightLbs: 300, Stackable: false},
	}
	v := testVehicle()
	p1 := s.Solve(v, items)
	p2 := s.Solve(v, items)
	if p1.TotalWeightLbs != p2.TotalWeightLbs || p1.BalanceScore != p2.BalanceScore {
		t.Fatalf("solver not deterministic: %+v vs %+v", p1, p2)
	}
	if len(p1.Placements) != len(p2.Placements) {
		t.Fatalf("placement count differs across runs")
	}
}
