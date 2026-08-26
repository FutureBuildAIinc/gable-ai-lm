// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// The yard proof-of-load gate (T1-6) says no truck leaves without a photo of
// how it was loaded AND a human sign-off. AttachProof already withdraws a
// sign-off when new evidence lands. These tests own the other half: a sign-off
// must not survive the truck being RE-PACKED, because the signature attests to
// an arrangement of the deck that no longer exists.
//
// The bug this pins: `packLoad` cleared `Compliance` on every re-pack but never
// touched `Proof`, so a signed-off truck could be re-packed by
//
//   - the dispatcher re-running Pack,
//   - a manual Resequence,
//   - the compliance reviewer's height-capped LOAD_ADJUST re-pack, or
//   - a cross-truck weight rebalance,
//
// and still clear the depart gate on a signature taken against the old load —
// different pack steps, a different securement plan, sometimes different cargo.

func intPtr(i int) *int         { return &i }
func f64Ptr(f float64) *float64 { return &f }
func testVehicles() []gable.Vehicle {
	return []gable.Vehicle{{
		ID: "v1", Name: "Flatbed 1", VehicleType: "FLATBED", CapacityWeightLbs: intPtr(20000),
	}}
}

// signedTwoStopPlan is a REVIEWED plan whose single truck carries two stops and
// is fully signed off: the state one drag away from a legal departure.
func signedTwoStopPlan() *Plan {
	p := planWithPackedLoad()
	p.Orders = append(p.Orders, OrderAnalysis{
		OrderID: "o2", CustomerName: "Beta",
		Lat: fptr(49.2), Lng: fptr(-119.2), Routable: true, TotalWeightLbs: 2000,
		Lines: []AnalyzedLine{{
			ProductID: "p2", SKU: "2x6-12", Quantity: 40,
			UnitWeightLbs: 50, UnitLengthIn: 144, UnitWidthIn: 5.5, UnitHeightIn: 1.5,
			Stackable: true, HasGeometry: true, LineWeightLbs: 2000,
		}},
	})
	p.Loads[0].Stops = append(p.Loads[0].Stops,
		Stop{OrderID: "o2", Sequence: 2, Lat: 49.2, Lng: -119.2, WeightLbs: 2000})

	signedAt := time.Date(2026, 6, 26, 7, 30, 0, 0, time.UTC)
	p.Loads[0].Proof.SignedOff = true
	p.Loads[0].Proof.SignedBy = "Yard Lead"
	p.Loads[0].Proof.SignedRole = "YARD"
	p.Loads[0].Proof.SignedAt = &signedAt
	return p
}

// TestRepackWithdrawsTheYardSignOff drives each path that re-solves a truck's
// packing and asserts the signature does not carry over.
func TestRepackWithdrawsTheYardSignOff(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, svc *Service) *Plan
	}{
		{
			name: "the dispatcher re-runs Pack",
			run: func(t *testing.T, svc *Service) *Plan {
				t.Helper()
				got, err := svc.Pack(context.Background(), "plan-1")
				if err != nil {
					t.Fatalf("pack: %v", err)
				}
				return got
			},
		},
		{
			name: "the dispatcher re-sequences the route",
			run: func(t *testing.T, svc *Service) *Plan {
				t.Helper()
				got, err := svc.Resequence(context.Background(), "plan-1", "v1", []string{"o2", "o1"}, false, "")
				if err != nil {
					t.Fatalf("resequence: %v", err)
				}
				return got
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePlanStore(signedTwoStopPlan())
			svc := newTestService(store, &fakeGable{vehicles: testVehicles()}, Config{})

			got := tc.run(t, svc)

			proof := got.Loads[0].Proof
			if proof == nil {
				t.Fatal("the proof record itself must survive — only the attestation is withdrawn")
			}
			if proof.SignedOff {
				t.Error("a re-pack changed the physical load; the yard sign-off must not carry over to it")
			}
			if proof.SignedAt != nil {
				t.Error("a withdrawn sign-off must not keep its timestamp")
			}
			if len(proof.Attachments) != 1 {
				t.Errorf("the yard's photos are evidence and must be kept, got %d attachments", len(proof.Attachments))
			}
			if proof.Ready() {
				t.Error("Proof.Ready() must be false, or the depart gate still opens")
			}
			// And it must actually be persisted, not just returned.
			if store.stored("plan-1").Loads[0].Proof.SignedOff {
				t.Error("the withdrawn sign-off was not written back to the store")
			}
		})
	}
}

// TestPushRefusesAfterARepackedLoadLosesItsSignOff is the consequence that
// matters: the depart gate must close again.
func TestPushRefusesAfterARepackedLoadLosesItsSignOff(t *testing.T) {
	store := newFakePlanStore(signedTwoStopPlan())
	g := &fakeGable{vehicles: testVehicles()}
	svc := newTestService(store, g, Config{})

	if _, err := svc.Resequence(context.Background(), "plan-1", "v1", []string{"o2", "o1"}, false, ""); err != nil {
		t.Fatalf("resequence: %v", err)
	}
	// Resequence also invalidates the review, so restore it — this test is about
	// the sign-off and nothing else.
	p, err := store.Get(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	p.Loads[0].Compliance = &ComplianceReview{Status: "PASS"}
	if err := store.Update(context.Background(), p); err != nil {
		t.Fatalf("update: %v", err)
	}

	if _, err := svc.Push(context.Background(), "plan-1"); err == nil {
		t.Fatal("push must refuse a truck whose sign-off was withdrawn by the re-pack")
	} else if !strings.Contains(err.Error(), "proof + sign-off required") {
		t.Fatalf("the refusal should name the missing sign-off, got %q", err.Error())
	}
	if len(g.pushed) != 0 {
		t.Fatal("nothing may reach the dispatch board on a withdrawn sign-off")
	}
}

// TestComplianceHeightCapRepackWithdrawsTheSignOff is the sharpest case,
// because no human asked for it: the reviewer re-packs the truck under a low
// overpass all by itself, and the signature was for the taller load.
func TestComplianceHeightCapRepackWithdrawsTheSignOff(t *testing.T) {
	store := newFakePlanStore(signedTwoStopPlan())
	svc := newTestService(store, &fakeGable{vehicles: testVehicles()}, Config{})

	// Pack for real first, so the reviewer's re-pack has a solved plan to
	// differ from and the sign-off is re-applied to THAT plan.
	if _, err := svc.Pack(context.Background(), "plan-1"); err != nil {
		t.Fatalf("pack: %v", err)
	}
	p, err := store.Get(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	signedAt := time.Date(2026, 6, 26, 7, 30, 0, 0, time.UTC)
	p.Loads[0].Proof.SignedOff = true
	p.Loads[0].Proof.SignedBy = "Yard Lead"
	p.Loads[0].Proof.SignedAt = &signedAt
	if err := store.Update(context.Background(), p); err != nil {
		t.Fatalf("update: %v", err)
	}

	// A low overpass sitting ON the second stop, so it cannot be routed around
	// and the reviewer's only move is to re-pack the load shorter. 100 in of
	// clearance leaves 40 in of cargo above a 58 in deck (less the 2 in margin),
	// which is below this load's packed height — so the re-pack genuinely
	// rearranges the deck rather than reproducing it.
	checker := &fakeChecker{result: &compliance.RouteCheckResult{
		Status: "FAIL",
		Flags: []compliance.Flag{{
			Point: compliance.RestrictedPoint{
				ID: "rp-1", Name: "Mill Creek underpass", Lat: 49.2, Lng: -119.2,
				RestrictionType: "HEIGHT", MaxHeightIn: f64Ptr(100),
			},
			Severity:  "FAIL",
			Violation: "load height exceeds clearance",
		}},
	}}
	svc = NewService(store, &fakeGable{vehicles: testVehicles()}, &fakeCatalog{}, &fakeFleet{}, checker, fakeBriefer{}, Config{})

	got, err := svc.Review(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("review: %v", err)
	}

	var adjusted bool
	for _, a := range got.Loads[0].Compliance.Actions {
		if a.Type == "LOAD_ADJUST" {
			adjusted = true
		}
	}
	if !adjusted {
		t.Fatalf("expected the reviewer to re-pack under the clearance cap; actions were %+v",
			got.Loads[0].Compliance.Actions)
	}
	if got.Loads[0].LoadPlan.MaxLoadHeightIn > 40 {
		t.Fatalf("the re-pack did not honour the 40 in cargo cap (height %.1f in), so this case is not exercising a changed load",
			got.Loads[0].LoadPlan.MaxLoadHeightIn)
	}
	if got.Loads[0].Proof.SignedOff {
		t.Error("the reviewer re-packed the truck under a clearance cap; the sign-off for the taller load must not carry over")
	}
}

// TestIdenticalRepackKeepsTheSignOff is the other half of the contract, and the
// reason the check compares plans rather than firing on every solve: re-running
// Pack on an unchanged order (a double-clicked button) produces the same load,
// so it must not send the yard chasing a fresh signature for nothing.
func TestIdenticalRepackKeepsTheSignOff(t *testing.T) {
	store := newFakePlanStore(signedTwoStopPlan())
	svc := newTestService(store, &fakeGable{vehicles: testVehicles()}, Config{})

	// First Pack replaces the hand-built fixture plan with a real solve.
	if _, err := svc.Pack(context.Background(), "plan-1"); err != nil {
		t.Fatalf("first pack: %v", err)
	}
	p, err := store.Get(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	signedAt := time.Date(2026, 6, 26, 7, 30, 0, 0, time.UTC)
	p.Loads[0].Proof.SignedOff = true
	p.Loads[0].Proof.SignedBy = "Yard Lead"
	p.Loads[0].Proof.SignedAt = &signedAt
	if err := store.Update(context.Background(), p); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Second Pack: same orders, same fleet, deterministic solver ⇒ same load.
	got, err := svc.Pack(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("second pack: %v", err)
	}
	if !got.Loads[0].Proof.SignedOff {
		t.Error("a re-pack that produced an identical load must leave the sign-off alone")
	}
	if got.Loads[0].Proof.SignedAt == nil {
		t.Error("an untouched sign-off must keep its timestamp")
	}
}
