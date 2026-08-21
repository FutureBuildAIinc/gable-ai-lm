// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/load"
)

// pushReadyPlan is a plan that clears every gate: reviewed, packed, within its
// GVW/axle ratings, nothing dropped, proof attached and signed off.
func pushReadyPlan() *Plan {
	p := planWithPackedLoad()
	p.Loads[0].Proof.SignedOff = true
	return p
}

// TestPushSucceedsWhenEveryGateClears is the positive control: it proves the
// refusals below come from the specific defect under test and not from a gate
// that simply blocks everything. It also pins that a WARN (loaded near, but
// within, the rating) is legal to dispatch.
func TestPushSucceedsWhenEveryGateClears(t *testing.T) {
	for _, status := range []string{"PASS", "WARN"} {
		t.Run(status, func(t *testing.T) {
			p := pushReadyPlan()
			p.Loads[0].LoadPlan.GVWStatus = status
			p.Loads[0].LoadPlan.AxleLoads[1].Status = status
			store := newFakePlanStore(p)
			g := &fakeGable{}

			got, err := newTestService(store, g, Config{}).Push(context.Background(), "plan-1")
			if err != nil {
				t.Fatalf("a load within its rating (%s) must be pushable: %v", status, err)
			}
			if got.Status != StatusPushed {
				t.Fatalf("plan status = %q, want %q", got.Status, StatusPushed)
			}
			if len(g.pushed) != 1 {
				t.Fatalf("expected 1 route on the dispatch board, got %d", len(g.pushed))
			}
		})
	}
}

// droppedCargo is cargo the packer HAD geometry for and could not fit. It stays
// in the yard, so the customer ships short: blocking.
func droppedCargo(skus ...string) []load.Unplaced {
	out := make([]load.Unplaced, 0, len(skus))
	for _, s := range skus {
		out = append(out, load.Unplaced{SKU: s, Quantity: 1, Reason: load.ReasonTruckFull, Detail: "truck full"})
	}
	return out
}

// dimensionlessCargo is cargo with no recorded geometry: there is nothing to
// place, but it still rides. Informational, never blocking.
func dimensionlessCargo(sku string, qty int, weightLbs float64) []load.Unplaced {
	return []load.Unplaced{{
		SKU: sku, Quantity: qty, Reason: load.ReasonNoGeometry,
		Detail: "no geometry recorded", WeightLbs: weightLbs,
	}}
}

// TestPushRefusesUncertifiedCapacity is the regression guard for the
// incomplete push gate. The route crosses no restricted point (compliance
// PASS) and the yard has signed off, so ONLY the truck's own load solve stands
// between an unsafe load and the live ERP dispatch board.
//
// The gate is a whitelist: PASS/WARN clear, everything else blocks. That covers
// an UNKNOWN status for an unrated axle and an empty status — the solver skips
// the GVW check entirely when the profile carries no GVWR, which would otherwise
// read as a confident PASS.
//
// The one thing NOT here is an over-rating on the solver's own advisory per-axle
// split; TestPushDoesNotRefuseOnTheAdvisoryAxleSplit below owns that case and
// explains why.
func TestPushRefusesUncertifiedCapacity(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(lp *load.Plan)
		wantText string
	}{
		{
			name:     "GVW over the truck's own rating",
			mutate:   func(lp *load.Plan) { lp.GVWStatus = "FAIL" },
			wantText: "GVW FAIL",
		},
		{
			// Advisory=false is the seam for a calibrated / scale-ticket-backed
			// per-axle source. A MEASURED over-rating is not an estimate, so it
			// still refuses — and the zero value of the flag is this one, so
			// anything that does not positively claim to be advisory blocks.
			name: "axle over a certified (non-advisory) rating",
			mutate: func(lp *load.Plan) {
				lp.AxleLoads[1].Status = "FAIL"
				lp.AxleLoads[1].Advisory = false
			},
			wantText: "axle 2 FAIL",
		},
		{
			name:     "axle rating could not be verified",
			mutate:   func(lp *load.Plan) { lp.AxleLoads[0].Status = "UNKNOWN" },
			wantText: "axle 1 UNKNOWN",
		},
		{
			name:     "GVW never evaluated (no GVWR on the profile)",
			mutate:   func(lp *load.Plan) { lp.GVWStatus = "" },
			wantText: "GVW UNKNOWN (not evaluated)",
		},
		{
			name:     "cargo the packer could not fit",
			mutate:   func(lp *load.Plan) { lp.Unplaced = droppedCargo("STONE-STEP-72", "WINDOW-CRATE-A") },
			wantText: "did not fit and were dropped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pushReadyPlan()
			tc.mutate(p.Loads[0].LoadPlan)
			store := newFakePlanStore(p)
			g := &fakeGable{}

			_, err := newTestService(store, g, Config{}).Push(context.Background(), "plan-1")
			if err == nil {
				t.Fatal("push must refuse a truck whose own load solve is not cleared")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q should name the reason %q", err.Error(), tc.wantText)
			}
			if len(g.pushed) != 0 {
				t.Fatal("nothing may reach the GableLBM dispatch board when the gate refuses")
			}
			if got := store.stored("plan-1").Status; got == StatusPushed {
				t.Fatal("a refused push must not mark the plan PUSHED")
			}
		})
	}
}

// TestPushDoesNotRefuseADimensionlessSku is the regression guard for defect 1:
// a SKU with no recorded geometry (a box of joist hangers, a tube of caulk) is
// an INFORMATION gap, not a capacity failure. The packer has nothing to place,
// which is not the same statement as "this would not fit on the truck".
//
// Before the fix the gate folded every unplaced entry into "N SKU(s) did not fit
// and were dropped" and refused, so any dealer who stocks joist hangers could
// never push a plan.
func TestPushDoesNotRefuseADimensionlessSku(t *testing.T) {
	p := pushReadyPlan()
	p.Loads[0].LoadPlan.Unplaced = dimensionlessCargo("HANGER-26", 120, 90)
	p.Loads[0].LoadPlan.UnmodeledWeightLbs = 90
	store := newFakePlanStore(p)
	g := &fakeGable{}

	got, err := newTestService(store, g, Config{}).Push(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("a plan carrying a dimensionless SKU must still push: %v", err)
	}
	if got.Status != StatusPushed {
		t.Fatalf("plan status = %q, want %q", got.Status, StatusPushed)
	}
	if len(g.pushed) != 1 {
		t.Fatalf("expected the route on the dispatch board, got %d", len(g.pushed))
	}

	// Not blocking is only half of it: the yard must be told, or the hangers
	// silently leave the manifest.
	m, ok := g.pushed[0].LoadManifest.(map[string]any)
	if !ok {
		t.Fatalf("manifest is %T, want a map", g.pushed[0].LoadManifest)
	}
	noGeom, _ := m["no_geometry"].([]string)
	if len(noGeom) != 1 || !strings.Contains(noGeom[0], "HANGER-26") {
		t.Fatalf("manifest must list the un-modelled lines, got %#v", m["no_geometry"])
	}
	if dropped, _ := m["dropped"].([]string); len(dropped) != 0 {
		t.Fatalf("a dimensionless SKU is NOT dropped cargo, got %#v", dropped)
	}
	if cleared, _ := m["capacity_cleared"].(bool); !cleared {
		t.Fatal("capacity is cleared: nothing failed to fit")
	}
	advisories, _ := m["advisories"].([]string)
	if len(advisories) == 0 {
		t.Fatal("the manifest must carry the no-geometry advisory — an advisory nobody sees is worse than none")
	}

	// Control: the SAME plan with the same SKU count reported as cargo that did
	// not FIT must still refuse. The only difference is the typed reason.
	blocked := pushReadyPlan()
	blocked.Loads[0].LoadPlan.Unplaced = droppedCargo("HANGER-26")
	if _, err := newTestService(newFakePlanStore(blocked), &fakeGable{}, Config{}).
		Push(context.Background(), "plan-1"); err == nil {
		t.Fatal("control: cargo that genuinely did not fit must still block the push")
	}
}

// TestPushDoesNotRefuseOnTheAdvisoryAxleSplit is the regression guard for
// defect 2 — and it deliberately reverses what this suite used to assert.
//
// THE OLD EXPECTATION WAS WRONG. It pinned "any per-axle FAIL refuses a push",
// but load's package doc states that the per-axle split is advisory: the datum
// puts the steer axle at the bed origin, overhang lever reactions are not
// modelled, and tare is apportioned by axle rating rather than chassis CG.
// ROADMAP §3 says the same and adds that it cannot be validated without
// certified scale tickets. Blocking on that number turned an unvalidated
// estimate into a hard cap at roughly half of a flatbed's rated payload — and
// the gate contradicted the very doc that describes the number.
//
// So: warn loudly, do not refuse. GVW stays exact and stays blocking, which
// TestPushRefusesUncertifiedCapacity above still proves.
func TestPushDoesNotRefuseOnTheAdvisoryAxleSplit(t *testing.T) {
	p := pushReadyPlan()
	p.Loads[0].LoadPlan.AxleLoads[0] = load.AxleLoad{
		AxleNumber: 1, WeightLbs: 13_400, MaxWeightLbs: 12_000,
		Utilization: 1.117, Status: "FAIL", Advisory: true,
	}
	store := newFakePlanStore(p)
	g := &fakeGable{}

	got, err := newTestService(store, g, Config{}).Push(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("an ADVISORY axle over-rating must not refuse a dispatch: %v", err)
	}
	if got.Status != StatusPushed {
		t.Fatalf("plan status = %q, want %q", got.Status, StatusPushed)
	}

	// It must not vanish either. The manifest that reaches the yard carries it.
	m := g.pushed[0].LoadManifest.(map[string]any)
	advisories, _ := m["advisories"].([]string)
	found := false
	for _, a := range advisories {
		if strings.Contains(a, "axle 1") && strings.Contains(strings.ToUpper(a), "ADVISORY") &&
			strings.Contains(a, "certified scale") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the axle advisory must reach the manifest, naming the axle and the scale caveat: %#v", advisories)
	}
	if adv, _ := m["axle_loads_advisory"].(bool); !adv {
		t.Error("the manifest must declare the per-axle numbers advisory")
	}

	// GVW is the exact number and is NOT softened by any of this.
	over := pushReadyPlan()
	over.Loads[0].LoadPlan.GVWStatus = "FAIL"
	if _, err := newTestService(newFakePlanStore(over), &fakeGable{}, Config{}).
		Push(context.Background(), "plan-1"); err == nil {
		t.Fatal("control: an over-GVWR truck must still be refused")
	}
}

// TestReviewSurfacesAdvisoriesWithoutFailing pins where the dispatcher meets an
// advisory: the review step, which is the screen they read before pushing. It
// must be neither green (they would never see it) nor FAIL (that is the refusal
// this change removed).
func TestReviewSurfacesAdvisoriesWithoutFailing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(lp *load.Plan)
		want   string
	}{
		{"advisory axle over rating", func(lp *load.Plan) {
			lp.AxleLoads[0].Status = "FAIL"
			lp.AxleLoads[0].Advisory = true
		}, "axle 1"},
		{"line with no geometry", func(lp *load.Plan) {
			lp.Unplaced = dimensionlessCargo("HANGER-26", 120, 90)
			lp.UnmodeledWeightLbs = 90
		}, "HANGER-26"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pushReadyPlan()
			tc.mutate(p.Loads[0].LoadPlan)
			svc := NewService(newFakePlanStore(p), &fakeGable{}, &fakeCatalog{}, &fakeFleet{},
				&fakeChecker{result: &compliance.RouteCheckResult{Status: "PASS", Flags: []compliance.Flag{}}},
				fakeBriefer{}, Config{})

			got, err := svc.Review(context.Background(), "plan-1")
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			review := got.Loads[0].Compliance
			if review.Status != "WARN" {
				t.Fatalf("review status = %q, want WARN — an advisory is neither clear nor a refusal", review.Status)
			}
			if len(review.Advisories) == 0 {
				t.Fatal("the review must carry the advisory text for the UI to render")
			}
			joined := strings.Join(review.Advisories, " | ")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("advisory %q should name %q", joined, tc.want)
			}
			for _, a := range review.Actions {
				if a.Type == "MANUAL_REVIEW" && strings.Contains(a.Description, "capacity not cleared") {
					t.Fatal("an advisory must not raise a blocking capacity action")
				}
			}
		})
	}
}

// TestPushStillRefusesRestrictedPointFailAndMissingSignOff guards the two gates
// that already existed, so the new capacity gate did not displace them.
func TestPushStillRefusesRestrictedPointFailAndMissingSignOff(t *testing.T) {
	t.Run("restricted-point FAIL", func(t *testing.T) {
		p := pushReadyPlan()
		p.Loads[0].Compliance.Status = "FAIL"
		_, err := newTestService(newFakePlanStore(p), nil, Config{}).Push(context.Background(), "plan-1")
		if err == nil || !strings.Contains(err.Error(), "compliance FAIL") {
			t.Fatalf("expected a compliance refusal, got %v", err)
		}
	})
	t.Run("missing yard sign-off", func(t *testing.T) {
		p := pushReadyPlan()
		p.Loads[0].Proof.SignedOff = false
		_, err := newTestService(newFakePlanStore(p), nil, Config{}).Push(context.Background(), "plan-1")
		if err == nil || !strings.Contains(err.Error(), "sign-off") {
			t.Fatalf("expected a proof/sign-off refusal, got %v", err)
		}
	})
}

// TestBuildManifestSurfacesDroppedCargo verifies the yard-facing manifest
// carries the load's capacity verdict and any cargo that did not fit — without
// it, an auto-resolved load silently ships the customer short.
func TestBuildManifestSurfacesDroppedCargo(t *testing.T) {
	p := pushReadyPlan()
	p.Loads[0].LoadPlan.Unplaced = droppedCargo("STONE-STEP-72")
	p.Loads[0].LoadPlan.GVWStatus = "WARN"

	m := buildManifest(p, p.Loads[0])

	unplaced, ok := m["unplaced"].([]load.Unplaced)
	if !ok || len(unplaced) != 1 || unplaced[0].SKU != "STONE-STEP-72" {
		t.Fatalf("manifest must list dropped SKUs, got %#v", m["unplaced"])
	}
	dropped, ok := m["dropped"].([]string)
	if !ok || len(dropped) != 1 || !strings.Contains(dropped[0], "STONE-STEP-72") {
		t.Fatalf("manifest must separate cargo that did not fit, got %#v", m["dropped"])
	}
	if m["gvw_status"] != "WARN" {
		t.Fatalf("manifest gvw_status = %#v, want WARN", m["gvw_status"])
	}
	if m["axle_loads"] == nil {
		t.Fatal("manifest must carry per-axle loads/status")
	}
	if cleared, _ := m["capacity_cleared"].(bool); cleared {
		t.Fatal("capacity_cleared must be false when cargo was dropped")
	}

	// A clean load states the absence explicitly rather than omitting the key.
	clean := pushReadyPlan()
	cm := buildManifest(clean, clean.Loads[0])
	if u, ok := cm["unplaced"].([]load.Unplaced); !ok || len(u) != 0 {
		t.Fatalf("a clean manifest must carry an empty unplaced list, got %#v", cm["unplaced"])
	}
	if d, ok := cm["dropped"].([]string); !ok || len(d) != 0 {
		t.Fatalf("a clean manifest must state 'nothing dropped' explicitly, got %#v", cm["dropped"])
	}
	if cleared, _ := cm["capacity_cleared"].(bool); !cleared {
		t.Fatal("capacity_cleared must be true for a fully-cleared load")
	}
}

// TestReviewMarksCapacityFailure verifies the dispatcher is told BEFORE push:
// a truck over its own rating, or one carrying less than the order, reviews as
// FAIL with a MANUAL_REVIEW action even when the route itself is clean.
func TestReviewMarksCapacityFailure(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(lp *load.Plan)
		want    string
		wantAct bool
	}{
		{"axle over a certified rating", func(lp *load.Plan) {
			lp.AxleLoads[1].Status = "FAIL"
			lp.AxleLoads[1].Advisory = false
		}, "FAIL", true},
		{"dropped cargo", func(lp *load.Plan) { lp.Unplaced = droppedCargo("STONE-STEP-72") }, "FAIL", true},
		{"unrated axle", func(lp *load.Plan) { lp.AxleLoads[0].Status = "UNKNOWN" }, "FAIL", true},
		{"near rating", func(lp *load.Plan) { lp.GVWStatus = "WARN" }, "WARN", false},
		{"all clear", func(lp *load.Plan) {}, "PASS", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pushReadyPlan()
			tc.mutate(p.Loads[0].LoadPlan)
			store := newFakePlanStore(p)
			svc := NewService(store, &fakeGable{}, &fakeCatalog{}, &fakeFleet{},
				// The route itself is clean: any escalation comes from the load.
				&fakeChecker{result: &compliance.RouteCheckResult{Status: "PASS", Flags: []compliance.Flag{}}},
				fakeBriefer{}, Config{})

			got, err := svc.Review(context.Background(), "plan-1")
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			review := got.Loads[0].Compliance
			if review.Status != tc.want {
				t.Fatalf("review status = %q, want %q", review.Status, tc.want)
			}
			hasAction := false
			for _, a := range review.Actions {
				if a.Type == "MANUAL_REVIEW" && strings.Contains(a.Description, "capacity") {
					hasAction = true
				}
			}
			if hasAction != tc.wantAct {
				t.Fatalf("capacity MANUAL_REVIEW action present = %v, want %v (%+v)", hasAction, tc.wantAct, review.Actions)
			}
		})
	}
}

// TestCapacityStatusClearsIsAWhitelist pins the fail-closed rule directly, so a
// future status added by the load solver blocks by default instead of slipping
// through an enumeration of known-bad values.
func TestCapacityStatusClearsIsAWhitelist(t *testing.T) {
	for _, ok := range []string{"PASS", "WARN", "pass", " warn "} {
		if !capacityStatusClears(ok) {
			t.Errorf("%q should clear (within rating)", ok)
		}
	}
	for _, blocked := range []string{"FAIL", "UNKNOWN", "", "  ", "UNRATED", "OVERRIDDEN", "N/A"} {
		if capacityStatusClears(blocked) {
			t.Errorf("%q must NOT clear — anything but an explicit pass blocks", blocked)
		}
	}
}
