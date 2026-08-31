// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

func fptr(f float64) *float64 { return &f }
func sptr(s string) *string   { return &s }

// The dealer's yards, as GableLBM reports them. Plano has never been geocoded,
// so its coordinate keys are absent and arrive here as nil.
const (
	dallasYardID    = "77777777-7777-4777-8777-000000000001"
	planoYardID     = "77777777-7777-4777-8777-000000000003"
	closedYardID    = "77777777-7777-4777-8777-000000000009"
	dallasYardLat   = 32.7767
	dallasYardLng   = -96.7970
	austinConfigLat = 30.2672
	austinConfigLng = -97.7431
)

func routingBranches() []gable.Location {
	return []gable.Location{
		{ID: dallasYardID, Name: "Dallas Yard", Latitude: fptr(dallasYardLat), Longitude: fptr(dallasYardLng)},
		{ID: planoYardID, Name: "Plano Yard"}, // never geocoded — nil, NOT 0,0
	}
}

// Two Texas stops whose centroid is (32.80, -97.00).
func routingOrders() []gable.Order {
	return []gable.Order{
		{ID: "o1", Latitude: fptr(32.70), Longitude: fptr(-96.90),
			Lines: []gable.OrderLine{{ProductID: "p1", SKU: "2x4-8", Quantity: 10, WeightLbs: 10}}},
		{ID: "o2", Latitude: fptr(32.90), Longitude: fptr(-97.10),
			Lines: []gable.OrderLine{{ProductID: "p1", SKU: "2x4-8", Quantity: 10, WeightLbs: 10}}},
	}
}

// --- fakes -------------------------------------------------------------------

type fakeGable struct {
	orders    []gable.Order
	vehicles  []gable.Vehicle
	drivers   []gable.Driver
	locations []gable.Location
	locErr    error

	locationCalls int
}

func (f *fakeGable) ListOrdersForDate(context.Context, string) ([]gable.Order, error) {
	return f.orders, nil
}

func (f *fakeGable) ListVehicles(context.Context) ([]gable.Vehicle, error) {
	if f.vehicles == nil {
		return []gable.Vehicle{{ID: "v1", Name: "Truck 1", CapacityWeightLbs: capPtr(20000)}}, nil
	}
	return f.vehicles, nil
}

func (f *fakeGable) ListDrivers(context.Context) ([]gable.Driver, error) {
	if f.drivers == nil {
		return []gable.Driver{{ID: "d1", Name: "Pat", Status: "ACTIVE"}}, nil
	}
	return f.drivers, nil
}

func (f *fakeGable) ListLocations(context.Context) ([]gable.Location, error) {
	f.locationCalls++
	if f.locErr != nil {
		return nil, f.locErr
	}
	return f.locations, nil
}

func (f *fakeGable) PushDeliveryRoute(context.Context, gable.DeliveryRoute) error { return nil }

// fakeStore stands in for *Repository so planning can be exercised with no
// Postgres.
type fakeStore struct{ saved *Plan }

func (s *fakeStore) Save(_ context.Context, p *Plan) error {
	p.ID = "plan-1"
	s.saved = p
	return nil
}

func (s *fakeStore) Get(context.Context, string) (*Plan, error) { return s.saved, nil }

func (s *fakeStore) UpdateStatus(context.Context, string, string) error { return nil }

// recordingOptimizer captures the origin the plan was actually sequenced from —
// which is the only place the depot is observable, since route_plans has no
// column to persist it in.
type recordingOptimizer struct {
	lat, lng float64
	called   bool
}

func (r *recordingOptimizer) Sequence(_ context.Context, depotLat, depotLng float64, stops []Stop) ([]Stop, float64, float64) {
	r.lat, r.lng, r.called = depotLat, depotLng, true
	return optimizeSequence(depotLat, depotLng, stops)
}

func (r *recordingOptimizer) Name() string { return "recording" }

// planWith runs a plan and reports the origin its stops were sequenced from.
func planWith(t *testing.T, g *fakeGable, cfg Config, req PlanRequest) (*Plan, *recordingOptimizer) {
	t.Helper()
	rec := &recordingOptimizer{}
	prev := active
	active = rec
	t.Cleanup(func() { active = prev })

	svc := NewService(&fakeStore{}, g, g, g, g, g, cfg)
	plan, err := svc.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !rec.called {
		t.Fatal("no load was sequenced, so no origin was exercised")
	}
	return plan, rec
}

func assertOrigin(t *testing.T, rec *recordingOptimizer, wantLat, wantLng float64) {
	t.Helper()
	if math.Abs(rec.lat-wantLat) > 1e-6 || math.Abs(rec.lng-wantLng) > 1e-6 {
		t.Fatalf("plan rooted at (%v, %v), want (%v, %v)", rec.lat, rec.lng, wantLat, wantLng)
	}
}

// --- tests -------------------------------------------------------------------

// TestPlanRootsAtTheRequestedBranch is the bug this change exists for: the
// endpoint was handed an explicit branch id, stored it on the plan, and then
// rooted the route at the middle of the stops anyway. An explicit branch is the
// least ambiguous input there is — it must decide the origin.
func TestPlanRootsAtTheRequestedBranch(t *testing.T) {
	g := &fakeGable{orders: routingOrders(), locations: routingBranches()}
	plan, rec := planWith(t, g, Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)},
		PlanRequest{Date: "2026-06-26", BranchID: sptr(dallasYardID)})

	assertOrigin(t, rec, dallasYardLat, dallasYardLng)
	if plan.GableBranchID == nil || *plan.GableBranchID != dallasYardID {
		t.Fatalf("plan lost the branch it was asked for: %v", plan.GableBranchID)
	}
}

// TestPlanRequestDepotStillOutranksTheBranch: an explicit coordinate is the
// operator's own answer, so the branch is not even looked up.
func TestPlanRequestDepotStillOutranksTheBranch(t *testing.T) {
	g := &fakeGable{orders: routingOrders(), locations: routingBranches()}
	_, rec := planWith(t, g, Config{}, PlanRequest{
		Date: "2026-06-26", BranchID: sptr(dallasYardID),
		DepotLat: fptr(29.7604), DepotLng: fptr(-95.3698),
	})

	assertOrigin(t, rec, 29.7604, -95.3698)
	if g.locationCalls != 0 {
		t.Fatalf("the branch list was fetched %d times for a request that already named a depot", g.locationCalls)
	}
}

// TestPlanWithUnknownBranchFallsBackRatherThanFailing: an inactive branch is
// simply absent from GableLBM's list. That is a fallback, not a 502.
func TestPlanWithUnknownBranchFallsBackRatherThanFailing(t *testing.T) {
	g := &fakeGable{orders: routingOrders(), locations: routingBranches()}
	_, rec := planWith(t, g, Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)},
		PlanRequest{Date: "2026-06-26", BranchID: sptr(closedYardID)})

	assertOrigin(t, rec, austinConfigLat, austinConfigLng)
}

// TestPlanWithUngeocodedBranchFallsBackRatherThanRootingAtNull: Plano exists
// but has never been geocoded. nil must never become 0,0 — a route rooted in
// the Gulf of Guinea would look like an answer.
func TestPlanWithUngeocodedBranchFallsBackRatherThanRootingAtNull(t *testing.T) {
	g := &fakeGable{orders: routingOrders(), locations: routingBranches()}
	_, rec := planWith(t, g, Config{}, PlanRequest{Date: "2026-06-26", BranchID: sptr(planoYardID)})

	if rec.lat == 0 && rec.lng == 0 {
		t.Fatal("an ungeocoded branch was taken as (0,0)")
	}
	// Nothing configured, so the run falls all the way to its own stops.
	assertOrigin(t, rec, 32.80, -97.00)
}

// TestPlanSurvivesAGableWithoutTheLocationsEndpoint: AI_LM and GableLBM deploy
// separately. An ERP that has not shipped /api/integration/locations answers
// 404, and that must cost the plan its branch origin, not the plan.
func TestPlanSurvivesAGableWithoutTheLocationsEndpoint(t *testing.T) {
	g := &fakeGable{
		orders: routingOrders(),
		locErr: errors.New("gable GET /api/integration/locations: status 404"),
	}
	_, rec := planWith(t, g, Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)},
		PlanRequest{Date: "2026-06-26", BranchID: sptr(dallasYardID)})

	assertOrigin(t, rec, austinConfigLat, austinConfigLng)
}

// TestPlanWithConfiguredDepotOutranksTheCentroid: the routing endpoint used to
// have no CONFIG rung at all, so an install with a configured yard still had
// its routes rooted at the middle of the day's stops.
func TestPlanWithConfiguredDepotOutranksTheCentroid(t *testing.T) {
	g := &fakeGable{orders: routingOrders()}
	_, rec := planWith(t, g, Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)},
		PlanRequest{Date: "2026-06-26"})

	assertOrigin(t, rec, austinConfigLat, austinConfigLng)
	if g.locationCalls != 0 {
		t.Fatalf("the branch list was fetched %d times for a request naming no branch", g.locationCalls)
	}
}

// TestPlanWithNothingConfiguredStillUsesTheStopCentroid pins the unchanged
// behaviour of an install with no depot and no branch: the day's own stops.
func TestPlanWithNothingConfiguredStillUsesTheStopCentroid(t *testing.T) {
	g := &fakeGable{orders: routingOrders()}
	_, rec := planWith(t, g, Config{}, PlanRequest{Date: "2026-06-26"})

	assertOrigin(t, rec, 32.80, -97.00)
}
