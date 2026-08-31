// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/catalog"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/fleet"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// In-memory doubles for the workflow's five seams plus its plan store, so the
// orchestrator can be exercised end-to-end with no Postgres and no live ERP.

// fakePlanStore reproduces the Postgres persistence contract in memory:
// every read hands back an independent copy (the real repository round-trips
// through JSONB, so callers can never alias stored state), and every write is
// guarded by the plan's optimistic-concurrency version exactly as
// `UPDATE ... WHERE id=$1 AND version=$expected` is.
type fakePlanStore struct {
	mu     sync.Mutex
	plans  map[string]*Plan
	nextID int

	updates   int
	conflicts int

	// beforeUpdate fires once, immediately before the next Update is applied,
	// letting a test land a competing writer inside another actor's
	// read-modify-write window (two dispatch users, two goroutines, one plan).
	beforeUpdate func()
}

func newFakePlanStore(seed ...*Plan) *fakePlanStore {
	s := &fakePlanStore{plans: map[string]*Plan{}}
	for _, p := range seed {
		if p.ID == "" {
			s.nextID++
			p.ID = fmt.Sprintf("plan-%d", s.nextID)
		}
		if p.Version == 0 {
			p.Version = 1
		}
		s.plans[p.ID] = clonePlan(p)
	}
	return s
}

func clonePlan(p *Plan) *Plan {
	raw, err := json.Marshal(p)
	if err != nil {
		panic("clonePlan: " + err.Error())
	}
	var out Plan
	if err := json.Unmarshal(raw, &out); err != nil {
		panic("clonePlan: " + err.Error())
	}
	return &out
}

func (s *fakePlanStore) Create(_ context.Context, p *Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	p.ID = fmt.Sprintf("plan-%d", s.nextID)
	p.Version = 1
	p.CreatedAt = time.Now()
	p.UpdatedAt = p.CreatedAt
	s.plans[p.ID] = clonePlan(p)
	return nil
}

func (s *fakePlanStore) Update(_ context.Context, p *Plan) error {
	if hook := s.takeHook(); hook != nil {
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.plans[p.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != p.Version {
		s.conflicts++
		return ErrVersionConflict
	}
	stored := clonePlan(p)
	stored.Version = cur.Version + 1
	stored.UpdatedAt = time.Now()
	s.plans[p.ID] = stored
	p.Version = stored.Version
	s.updates++
	return nil
}

func (s *fakePlanStore) takeHook() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	hook := s.beforeUpdate
	s.beforeUpdate = nil
	return hook
}

func (s *fakePlanStore) Get(_ context.Context, id string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clonePlan(p), nil
}

func (s *fakePlanStore) GetLatestForDate(_ context.Context, date string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.plans {
		if p.PlanDate == date {
			return clonePlan(p), nil
		}
	}
	return nil, ErrNotFound
}

// stored returns the currently persisted plan (test-only accessor).
func (s *fakePlanStore) stored(id string) *Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clonePlan(s.plans[id])
}

// errPlanStore fails every write with a fixed error (for handler mapping).
type errPlanStore struct {
	inner *fakePlanStore
	err   error
}

func (s errPlanStore) Create(ctx context.Context, p *Plan) error { return s.inner.Create(ctx, p) }
func (s errPlanStore) Update(context.Context, *Plan) error       { return s.err }
func (s errPlanStore) Get(ctx context.Context, id string) (*Plan, error) {
	return s.inner.Get(ctx, id)
}
func (s errPlanStore) GetLatestForDate(ctx context.Context, d string) (*Plan, error) {
	return s.inner.GetLatestForDate(ctx, d)
}

// fakeGable is the GableLBM integration double. pushed records every route
// written back so a test can assert what actually reached the dispatch board.
type fakeGable struct {
	orders    []gable.Order
	vehicles  []gable.Vehicle
	drivers   []gable.Driver
	locations []gable.Location

	// locErr simulates a GableLBM that cannot answer the branch lookup —
	// including one that predates /api/integration/locations entirely.
	locErr error

	// locationCalls counts branch lookups. The lookup is a round-trip to the
	// ERP on every ingest, so "was it even asked?" is a behaviour worth
	// asserting, not just an implementation detail.
	locationCalls int

	pushed  []gable.DeliveryRoute
	pushErr error

	// pushErrAfter, when > 0, lets the Nth push and every one before it
	// succeed and fails the rest — the mid-loop failure a partial push is.
	pushErrAfter int

	// recalled records every route withdrawal, in order, so a test can assert
	// that an override recalled EXACTLY the trucks the re-assignment dropped
	// and no others. Recalling a surviving truck would cancel a good route,
	// which is the regression this record exists to catch.
	recalled []gable.RouteRecall
	// recallErr fails every recall (a GableLBM that cannot be reached);
	// recallDispatched fails them with the terminal 409 instead.
	recallErr        error
	recallDispatched bool
	// recallMissing marks the recall as having found nothing on the board —
	// the idempotent no-op, which is a success.
	recallMissing bool
}

func (f *fakeGable) ListOrdersForDate(context.Context, string) ([]gable.Order, error) {
	return f.orders, nil
}
func (f *fakeGable) ListVehicles(context.Context) ([]gable.Vehicle, error) { return f.vehicles, nil }
func (f *fakeGable) ListLocations(context.Context) ([]gable.Location, error) {
	f.locationCalls++
	if f.locErr != nil {
		return nil, f.locErr
	}
	return f.locations, nil
}
func (f *fakeGable) ListDrivers(context.Context) ([]gable.Driver, error) { return f.drivers, nil }
func (f *fakeGable) PushDeliveryRoute(_ context.Context, r gable.DeliveryRoute) error {
	if f.pushErrAfter > 0 && len(f.pushed) >= f.pushErrAfter {
		return fmt.Errorf("gable POST /api/integration/delivery-routes: status 503: upstream unavailable")
	}
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = append(f.pushed, r)
	return nil
}

func (f *fakeGable) RecallDeliveryRoute(_ context.Context, rc gable.RouteRecall) (*gable.RouteRecallResult, error) {
	if f.recallDispatched {
		return nil, fmt.Errorf("%w (truck %s on %s)", gable.ErrRouteDispatched, rc.VehicleID, rc.ScheduledDate)
	}
	if f.recallErr != nil {
		return nil, f.recallErr
	}
	f.recalled = append(f.recalled, rc)
	if f.recallMissing {
		return &gable.RouteRecallResult{Recalled: false}, nil
	}
	// Drop the recalled route from the board the fake is standing in for, so
	// "what is live upstream" stays honest across a push/recall/push cycle.
	kept := f.pushed[:0]
	for _, r := range f.pushed {
		if r.VehicleID != rc.VehicleID || r.ScheduledDate != rc.ScheduledDate {
			kept = append(kept, r)
		}
	}
	stops := 0
	if n := len(f.pushed) - len(kept); n > 0 {
		stops = 1
	}
	f.pushed = kept
	return &gable.RouteRecallResult{Recalled: stops > 0, RouteID: "route-" + rc.VehicleID, StopCount: stops}, nil
}

// recalledIDs lists the vehicles recalled, in call order.
func (f *fakeGable) recalledIDs() []string {
	out := make([]string, 0, len(f.recalled))
	for _, r := range f.recalled {
		out = append(out, r.VehicleID)
	}
	return out
}

// pushedIDs lists the vehicles currently live on the fake dispatch board.
func (f *fakeGable) pushedIDs() []string {
	out := make([]string, 0, len(f.pushed))
	for _, r := range f.pushed {
		out = append(out, r.VehicleID)
	}
	return out
}

// fakeCatalog resolves products to effective geometry.
type fakeCatalog struct{ products []catalog.EffectiveProduct }

func (f *fakeCatalog) ListEffectiveProducts(context.Context) ([]catalog.EffectiveProduct, error) {
	return f.products, nil
}

// fakeFleet serves stored vehicle profiles (ErrNotFound ⇒ auto-provision).
type fakeFleet struct{ profiles map[string]*fleet.Profile }

func (f *fakeFleet) GetProfile(_ context.Context, id string) (*fleet.Profile, error) {
	if p, ok := f.profiles[id]; ok {
		return p, nil
	}
	return nil, fleet.ErrNotFound
}

func (f *fakeFleet) UpsertProfile(_ context.Context, id string, in fleet.ProfileInput) (*fleet.Profile, error) {
	if f.profiles == nil {
		f.profiles = map[string]*fleet.Profile{}
	}
	p := &fleet.Profile{
		GableVehicleID: id,
		Name:           in.Name,
		BedLengthIn:    in.BedLengthIn,
		BedWidthIn:     in.BedWidthIn,
		BedHeightIn:    in.BedHeightIn,
		GVWRLbs:        in.GVWRLbs,
		TareWeightLbs:  in.TareWeightLbs,
	}
	for _, a := range in.Axles {
		p.Axles = append(p.Axles, fleet.Axle{
			AxleNumber:          a.AxleNumber,
			MaxWeightLbs:        a.MaxWeightLbs,
			PositionFromFrontIn: a.PositionFromFrontIn,
			AxleType:            a.AxleType,
		})
	}
	f.profiles[id] = p
	return p, nil
}

// fakeChecker returns a canned restricted-point verdict.
type fakeChecker struct {
	result *compliance.RouteCheckResult
	calls  int
}

func (f *fakeChecker) CheckRoute(context.Context, compliance.RouteCheckRequest) (*compliance.RouteCheckResult, error) {
	f.calls++
	if f.result != nil {
		return f.result, nil
	}
	return &compliance.RouteCheckResult{Status: "PASS", Flags: []compliance.Flag{}}, nil
}

// fakeBriefer is an unconfigured AI client (the workflow must never need it).
type fakeBriefer struct{}

func (fakeBriefer) Configured() bool { return false }
func (fakeBriefer) Model() string    { return "" }
func (fakeBriefer) Generate(context.Context, string, string, int) (string, error) {
	return "", fmt.Errorf("not configured")
}

// newTestService wires a Service over the supplied store with inert doubles.
func newTestService(store planStore, g *fakeGable, cfg Config) *Service {
	if g == nil {
		g = &fakeGable{}
	}
	return NewService(store, g, &fakeCatalog{}, &fakeFleet{}, &fakeChecker{}, fakeBriefer{}, cfg)
}

func fptr(f float64) *float64 { return &f }
