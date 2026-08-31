// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/depot"
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

// --- Divergence from the workflow ingest -------------------------------------
//
// The tests below exist because extracting the shared ladder was not the same
// thing as making the two callers agree. Routing built the ladder's wanted-yard
// set from req.BranchID alone while workflow built it from the ORDERS, so the
// same order set planned through the two endpoints came out rooted in two
// different places — and the ladder's multi-yard guard, which workflow trips
// routinely, could not fire from routing at all.

// shippingFrom stamps a yard onto each order in turn, cycling through the ids
// given: one id means "this whole run leaves from one yard", two means it spans
// two. It is the routing twin of the workflow fixture of the same name, because
// the two modules must be asked the identical question.
func shippingFrom(in []gable.Order, branchIDs ...string) []gable.Order {
	out := make([]gable.Order, len(in))
	copy(out, in)
	for i := range out {
		out[i].BranchID = branchIDs[i%len(branchIDs)]
	}
	return out
}

// resolveWith exercises the depot decision itself. It has to: route_plans is
// column-backed and has nowhere to persist a source or a note, so the note —
// the only place the multi-yard condition is visible — is reachable only here
// and in the log line Plan writes from it.
func resolveWith(t *testing.T, g *fakeGable, cfg Config, req PlanRequest) (lat, lng float64, source, note string) {
	t.Helper()
	ctx := context.Background()
	orders, err := g.ListOrdersForDate(ctx, req.Date)
	if err != nil {
		t.Fatalf("orders: %v", err)
	}
	var pts []depot.Point
	for _, o := range orders {
		if o.Latitude != nil && o.Longitude != nil {
			pts = append(pts, depot.Point{Lat: *o.Latitude, Lng: *o.Longitude})
		}
	}
	svc := NewService(&fakeStore{}, g, g, g, g, g, cfg)
	return svc.resolveDepot(ctx, req, orders, pts)
}

// TestPlanRootsAtTheBranchItsOrdersShipFrom is divergence A. GableLBM stamps
// every order with the yard it ships from, and the workflow ingest roots the
// day there. This endpoint never looked at the orders it already had in hand:
// with no explicit branch_id on the request it dropped straight past the BRANCH
// rung and rooted an all-Dallas day at this install's Austin depot — the same
// orders, two endpoints, two origins hundreds of miles apart.
func TestPlanRootsAtTheBranchItsOrdersShipFrom(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID),
		locations: routingBranches(),
	}
	cfg := Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)}

	_, rec := planWith(t, g, cfg, PlanRequest{Date: "2026-06-26"})
	assertOrigin(t, rec, dallasYardLat, dallasYardLng)
	if g.locationCalls == 0 {
		t.Fatal("the branch list was never fetched, so the orders' own yard was never looked up")
	}

	lat, lng, source, note := resolveWith(t, g, cfg, PlanRequest{Date: "2026-06-26"})
	if source != depot.SourceBranch {
		t.Fatalf("depot source = %q, want %q — the orders all name one yard", source, depot.SourceBranch)
	}
	if math.Abs(lat-dallasYardLat) > 1e-6 || math.Abs(lng-dallasYardLng) > 1e-6 {
		t.Fatalf("depot = (%v, %v), want the Dallas yard", lat, lng)
	}
	if note != "" {
		t.Fatalf("nothing was declined, but the plan carries a note: %q", note)
	}
}

// TestPlanSpanningTwoBranchesFallsBackAndSaysWhy is divergence B. Because
// routing only ever handed the ladder nought or one id, the multi-yard guard
// could not fire from this module however the day's orders were spread. A
// dealer shipping from Dallas and Plano on one date got a plan rooted at one of
// them — or, before this, at the centroid — with no note at all, while the
// workflow ingest refused that exact shape and explained itself.
func TestPlanSpanningTwoBranchesFallsBackAndSaysWhy(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID, planoYardID),
		locations: routingBranches(),
	}
	cfg := Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)}

	lat, lng, source, note := resolveWith(t, g, cfg, PlanRequest{Date: "2026-06-26"})
	if source != depot.SourceConfig {
		t.Fatalf("depot source = %q, want %q — a run spanning two yards must not claim a branch origin",
			source, depot.SourceConfig)
	}
	if math.Abs(lat-dallasYardLat) < 1e-6 {
		t.Fatalf("a yard was silently picked out of two: depot = (%v, %v)", lat, lng)
	}
	for _, want := range []string{"2 different branches", "Dallas Yard", "Plano Yard", "configured depot"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to mention %q", note, want)
		}
	}
}

// TestExplicitBranchWinsButStillReportsTheSpan pins the rule for combining the
// two sources of truth. The request naming Dallas is the caller's own statement
// of which yard the plan is for, so it decides the origin — but asking for
// Dallas does not move the Plano stops, and a dispatcher handed a plan whose
// stops are half in another yard has to be told. The origin is the caller's;
// the span is still theirs to know.
func TestExplicitBranchWinsButStillReportsTheSpan(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID, planoYardID),
		locations: routingBranches(),
	}
	cfg := Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)}
	req := PlanRequest{Date: "2026-06-26", BranchID: sptr(dallasYardID)}

	_, rec := planWith(t, g, cfg, req)
	assertOrigin(t, rec, dallasYardLat, dallasYardLng)

	lat, lng, source, note := resolveWith(t, g, cfg, req)
	if source != depot.SourceBranch {
		t.Fatalf("depot source = %q, want %q — an explicit branch decides the origin", source, depot.SourceBranch)
	}
	if math.Abs(lat-dallasYardLat) > 1e-6 || math.Abs(lng-dallasYardLng) > 1e-6 {
		t.Fatalf("depot = (%v, %v), want the requested Dallas yard", lat, lng)
	}
	if note == "" {
		t.Fatal("the plan was rooted at Dallas while half its stops ship from Plano, and said nothing")
	}
	for _, want := range []string{"2 different branches", "Dallas Yard", "Plano Yard", "rooted at the branch named on the request"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to mention %q", note, want)
		}
	}
}

// TestExplicitBranchAgreeingWithTheOrdersIsSilent: the span sentence is a
// warning, not decoration. When the request and every order name the same yard
// there is nothing to warn about.
func TestExplicitBranchAgreeingWithTheOrdersIsSilent(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID),
		locations: routingBranches(),
	}
	_, _, source, note := resolveWith(t, g, Config{}, PlanRequest{Date: "2026-06-26", BranchID: sptr(dallasYardID)})

	if source != depot.SourceBranch {
		t.Fatalf("depot source = %q, want %q", source, depot.SourceBranch)
	}
	if note != "" {
		t.Fatalf("request and orders agree, but the plan carries a note: %q", note)
	}
}

// TestExplicitBranchOverridesTheOrdersOwnYard: the request outranks what the
// orders imply. A dispatcher planning tomorrow's Fort Worth run out of the
// Dallas yard is making a decision, not a mistake.
func TestExplicitBranchOverridesTheOrdersOwnYard(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), planoYardID),
		locations: routingBranches(),
	}
	lat, lng, source, _ := resolveWith(t, g, Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)},
		PlanRequest{Date: "2026-06-26", BranchID: sptr(dallasYardID)})

	if source != depot.SourceBranch {
		t.Fatalf("depot source = %q, want %q", source, depot.SourceBranch)
	}
	if math.Abs(lat-dallasYardLat) > 1e-6 || math.Abs(lng-dallasYardLng) > 1e-6 {
		t.Fatalf("depot = (%v, %v), want the requested Dallas yard, not the orders' Plano", lat, lng)
	}
}

// TestPlanWithNoOrdersHasNothingToRootOn and its ungeocoded sibling pin the
// behaviour that must NOT change now that the orders feed the ladder: a day
// with nothing routable still falls through exactly as it did.
func TestPlanWithNoOrdersHasNothingToRootOn(t *testing.T) {
	g := &fakeGable{orders: nil, locations: routingBranches()}

	lat, lng, source, note := resolveWith(t, g, Config{}, PlanRequest{Date: "2026-06-26"})
	if source != depot.SourceNone || lat != 0 || lng != 0 {
		t.Fatalf("depot = (%v, %v) source %q, want (0,0) %q", lat, lng, source, depot.SourceNone)
	}
	if note != "" {
		t.Fatalf("no order named a yard, so nothing was declined; note = %q", note)
	}
	if g.locationCalls != 0 {
		t.Fatalf("the branch list was fetched %d times for a day with no orders", g.locationCalls)
	}
}

// TestPlanWithNoGeocodedOrdersFallsToTheConfiguredYard: orders that cannot be
// driven to must not drag the centroid, and with none of them routable the
// configured yard is all that is left.
func TestPlanWithNoGeocodedOrdersFallsToTheConfiguredYard(t *testing.T) {
	g := &fakeGable{
		orders: []gable.Order{
			{ID: "o1", Lines: []gable.OrderLine{{ProductID: "p1", Quantity: 1, WeightLbs: 10}}},
			{ID: "o2", Lines: []gable.OrderLine{{ProductID: "p1", Quantity: 1, WeightLbs: 10}}},
		},
		locations: routingBranches(),
	}
	lat, lng, source, _ := resolveWith(t, g, Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)},
		PlanRequest{Date: "2026-06-26"})

	if source != depot.SourceConfig {
		t.Fatalf("depot source = %q, want %q", source, depot.SourceConfig)
	}
	if math.Abs(lat-austinConfigLat) > 1e-6 || math.Abs(lng-austinConfigLng) > 1e-6 {
		t.Fatalf("depot = (%v, %v), want the configured Austin yard", lat, lng)
	}
}

// TestRequestDepotStillSilencesEverything: an operator who handed this run its
// own coordinate has answered the question. The branch list is not fetched and
// no note is written — the same silence the workflow ingest keeps for the same
// input, which is the point of both modules sharing one ladder.
func TestRequestDepotStillSilencesEverything(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID, planoYardID),
		locations: routingBranches(),
	}
	lat, lng, source, note := resolveWith(t, g, Config{}, PlanRequest{
		Date: "2026-06-26", BranchID: sptr(dallasYardID),
		DepotLat: fptr(29.7604), DepotLng: fptr(-95.3698),
	})

	if source != depot.SourceRequest || math.Abs(lat-29.7604) > 1e-6 || math.Abs(lng-(-95.3698)) > 1e-6 {
		t.Fatalf("depot = (%v, %v) source %q, want the request's own coordinate", lat, lng, source)
	}
	if note != "" {
		t.Fatalf("an explicit coordinate outranks every yard; note = %q", note)
	}
	if g.locationCalls != 0 {
		t.Fatalf("the branch list was fetched %d times for a request that already named a depot", g.locationCalls)
	}
}
