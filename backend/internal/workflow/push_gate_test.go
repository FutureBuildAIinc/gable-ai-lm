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

// TestPushRefusesUncertifiedCapacity is the regression guard for the
// incomplete push gate. The route crosses no restricted point (compliance
// PASS) and the yard has signed off, so ONLY the truck's own load solve stands
// between an unsafe load and the live ERP dispatch board.
//
// The gate is a whitelist: PASS/WARN clear, everything else blocks. That covers
// today's FAIL, an UNKNOWN status for an unrated axle, and an empty status —
// the solver skips the GVW check entirely when the profile carries no GVWR,
// which would otherwise read as a confident PASS.
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
			name:     "axle over its rating",
			mutate:   func(lp *load.Plan) { lp.AxleLoads[1].Status = "FAIL" },
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
			mutate:   func(lp *load.Plan) { lp.Unplaced = []string{"STONE-STEP-72", "WINDOW-CRATE-A"} },
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
	p.Loads[0].LoadPlan.Unplaced = []string{"STONE-STEP-72"}
	p.Loads[0].LoadPlan.GVWStatus = "WARN"

	m := buildManifest(p, p.Loads[0])

	unplaced, ok := m["unplaced"].([]string)
	if !ok || len(unplaced) != 1 || unplaced[0] != "STONE-STEP-72" {
		t.Fatalf("manifest must list dropped SKUs, got %#v", m["unplaced"])
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
	if u, ok := cm["unplaced"].([]string); !ok || len(u) != 0 {
		t.Fatalf("a clean manifest must carry an empty unplaced list, got %#v", cm["unplaced"])
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
		{"axle over rating", func(lp *load.Plan) { lp.AxleLoads[1].Status = "FAIL" }, "FAIL", true},
		{"dropped cargo", func(lp *load.Plan) { lp.Unplaced = []string{"STONE-STEP-72"} }, "FAIL", true},
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
