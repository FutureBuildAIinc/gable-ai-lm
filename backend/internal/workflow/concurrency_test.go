// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/load"
)

// planWithPackedLoad builds a minimal plan carrying one packed, proof-bearing
// truck — the shape every yard/dispatcher mutation operates on.
func planWithPackedLoad() *Plan {
	return &Plan{
		ID:       "plan-1",
		PlanDate: "2026-06-26",
		Status:   StatusReviewed,
		Version:  1,
		DepotLat: 49.0,
		DepotLng: -119.0,
		Orders: []OrderAnalysis{{
			OrderID: "o1", CustomerName: "Acme",
			Lat: fptr(49.1), Lng: fptr(-119.1), Routable: true, TotalWeightLbs: 4000,
			Lines: []AnalyzedLine{{
				ProductID: "p1", SKU: "2x4-8", Quantity: 100,
				UnitWeightLbs: 40, UnitLengthIn: 96, UnitWidthIn: 3.5, UnitHeightIn: 1.5,
				Stackable: true, HasGeometry: true, LineWeightLbs: 4000,
			}},
		}},
		Loads: []TruckLoad{{
			VehicleID:         "v1",
			VehicleName:       "Flatbed 1",
			CapacityWeightLbs: 20000,
			TotalWeightLbs:    4000,
			Stops:             []Stop{{OrderID: "o1", Sequence: 1, Lat: 49.1, Lng: -119.1, WeightLbs: 4000}},
			// Shaped like real solver output: every axle rated, and every
			// per-axle verdict flagged Advisory (load.AxleLoad documents it as
			// always true). Tests that need a CERTIFIED per-axle verdict clear
			// Advisory explicitly — the gate keys on that flag.
			LoadPlan: &load.Plan{
				TotalWeightLbs: 18000,
				GVWStatus:      "PASS",
				AxleLoads: []load.AxleLoad{
					{AxleNumber: 1, WeightLbs: 6000, MaxWeightLbs: 12000, Utilization: 0.5, Status: "PASS", Advisory: true},
					{AxleNumber: 2, WeightLbs: 12000, MaxWeightLbs: 21000, Utilization: 0.571, Status: "PASS", Advisory: true},
				},
			},
			Compliance: &ComplianceReview{Status: "PASS"},
			Proof: &LoadProof{
				Attachments: []ProofAttachment{{URL: "https://yard/photo-1.jpg", Kind: "PHOTO"}},
			},
		}},
		UnassignedOrders: []Stop{},
	}
}

// TestMutationAdvancesPlanVersion verifies a normal mutation round-trips the
// optimistic-concurrency token: the service writes back the version it read and
// the store advances it. Without the token being carried on the model the write
// would be rejected (or, worse, be unguarded).
func TestMutationAdvancesPlanVersion(t *testing.T) {
	store := newFakePlanStore(planWithPackedLoad())
	svc := newTestService(store, nil, Config{})

	got, err := svc.SignOffLoad(context.Background(), "plan-1", "v1", SignOffRequest{SignedBy: "Yard Lead"})
	if err != nil {
		t.Fatalf("sign-off should succeed on an unconcurrent plan: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("mutation must advance the plan version 1→2, got %d", got.Version)
	}
	if v := store.stored("plan-1").Version; v != 2 {
		t.Fatalf("stored version must advance to 2, got %d", v)
	}
}

// TestConcurrentMutationRejectsStaleWrite is the regression guard for the
// lost-update blocker: a dispatcher and a yard lead act on the same plan at the
// same time (two net/http goroutines, one row — this happens at
// INSTANCE_COUNT=1). The yard lead reads the plan, the dispatcher's reroute
// commits first, and the yard lead's write must then be REFUSED rather than
// silently overwriting the reroute with its own stale document.
func TestConcurrentMutationRejectsStaleWrite(t *testing.T) {
	store := newFakePlanStore(planWithPackedLoad())
	svc := newTestService(store, nil, Config{})

	// The dispatcher's request lands between the yard lead's read and write.
	store.beforeUpdate = func() {
		p, err := store.Get(context.Background(), "plan-1")
		if err != nil {
			t.Errorf("competing read failed: %v", err)
			return
		}
		p.Loads[0].Stops[0].Sequence = 7 // the reroute
		if err := store.Update(context.Background(), p); err != nil {
			t.Errorf("competing write should succeed (it is the first writer): %v", err)
		}
	}

	_, err := svc.SignOffLoad(context.Background(), "plan-1", "v1", SignOffRequest{SignedBy: "Yard Lead"})
	if err == nil {
		t.Fatal("a write against a stale plan version must be refused, not silently applied")
	}
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict so the handler can answer 409, got %v", err)
	}

	final := store.stored("plan-1")
	if final.Loads[0].Stops[0].Sequence != 7 {
		t.Fatal("the dispatcher's reroute was lost — the stale write overwrote it")
	}
	if final.Loads[0].Proof.SignedOff {
		t.Fatal("the refused sign-off must not have been persisted")
	}
	if final.Version != 2 {
		t.Fatalf("only the winning write may advance the version, got %d", final.Version)
	}
}

// TestConcurrentPushRejectsStaleWrite covers the most dangerous mutation: two
// pushes racing must not both mark the plan PUSHED off a stale read.
func TestConcurrentPushRejectsStaleWrite(t *testing.T) {
	p := planWithPackedLoad()
	p.Loads[0].Proof.SignedOff = true
	store := newFakePlanStore(p)
	g := &fakeGable{}
	svc := newTestService(store, g, Config{})

	store.beforeUpdate = func() {
		cur, err := store.Get(context.Background(), "plan-1")
		if err != nil {
			t.Errorf("competing read failed: %v", err)
			return
		}
		cur.Status = StatusPacked // a concurrent re-pack invalidated the review
		if err := store.Update(context.Background(), cur); err != nil {
			t.Errorf("competing write failed: %v", err)
		}
	}

	if _, err := svc.Push(context.Background(), "plan-1"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("a push racing another write must conflict, got %v", err)
	}
	if got := store.stored("plan-1").Status; got != StatusPacked {
		t.Fatalf("the competing status change must survive, got %q", got)
	}
}

// TestVersionConflictRespondsHTTP409 verifies the sentinel reaches the client
// as 409 Conflict (so the UI can reload + retry) and that the 404 mapping is
// still intact.
func TestVersionConflictRespondsHTTP409(t *testing.T) {
	base := newFakePlanStore(planWithPackedLoad())
	h := NewHandler(newTestService(errPlanStore{inner: base, err: ErrVersionConflict}, nil, Config{}))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"sign-off conflict", http.MethodPost, "/api/v1/workflow/plans/plan-1/loads/v1/sign-off", `{"signed_by":"Yard Lead"}`, http.StatusConflict},
		{"proof conflict", http.MethodPost, "/api/v1/workflow/plans/plan-1/loads/v1/proof", `{"url":"https://yard/p.jpg"}`, http.StatusConflict},
		{"dimension override conflict", http.MethodPut, "/api/v1/workflow/plans/plan-1/orders/o1/dimensions", `{"sku":"2x4-8","length_in":96,"width_in":4,"height_in":2}`, http.StatusConflict},
		{"missing plan is still 404", http.MethodPost, "/api/v1/workflow/plans/nope/loads/v1/sign-off", `{"signed_by":"Yard Lead"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got HTTP %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
