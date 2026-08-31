// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Business metering (spec: "Incremented where the value ACTUALLY occurs — a
// plan that fails to persist must not count").
//
// These tests are about WHERE the counters move, not that they can move. A
// metering seam that counts attempts rather than outcomes is worse than none:
// it produces a number that looks authoritative, is used to bill or to size a
// contract, and is quietly wrong in the dealer's disfavour.
//
// Each test uses its own `subject` label so the counters are isolated from
// every other test in this package and from -race running them concurrently.

type meterProbe struct {
	m       *metrics.Meter
	edition string
	subject string
}

func newMeterProbe(t *testing.T, subject string) *meterProbe {
	t.Helper()
	p := &meterProbe{edition: "community", subject: subject}
	p.m = metrics.NewMeter(p.edition, p.subject)
	return p
}

func (p *meterProbe) plans(t *testing.T) float64  { return p.read(t, metrics.PlansCreatedTotal) }
func (p *meterProbe) trucks(t *testing.T) float64 { return p.read(t, metrics.TrucksPackedTotal) }
func (p *meterProbe) routes(t *testing.T) float64 { return p.read(t, metrics.RoutesPushedTotal) }

func (p *meterProbe) read(t *testing.T, vec *prometheus.CounterVec) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(p.edition, p.subject)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// --- ailm_plans_created_total ------------------------------------------------

func TestIngestMetersOnlyPlansThatPersisted(t *testing.T) {
	t.Run("a stored plan counts once", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-ingest-ok")
		svc := newTestService(newFakePlanStore(), &fakeGable{}, Config{}).WithMeter(probe.m)

		if _, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := probe.plans(t); got != 1 {
			t.Fatalf("ailm_plans_created_total = %v after one ingest, want 1", got)
		}
	})

	t.Run("a plan that failed to store does not count", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-ingest-store-failed")
		store := failingCreateStore{inner: newFakePlanStore(), err: errors.New("insert failed")}
		svc := newTestService(store, &fakeGable{}, Config{}).WithMeter(probe.m)

		if _, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"}); err == nil {
			t.Fatal("expected the ingest to fail")
		}
		if got := probe.plans(t); got != 0 {
			t.Fatalf("ailm_plans_created_total = %v, want 0 — a plan that never reached the database is not a plan", got)
		}
	})

	t.Run("a rejected request does not count", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-ingest-bad-request")
		svc := newTestService(newFakePlanStore(), &fakeGable{}, Config{}).WithMeter(probe.m)

		if _, err := svc.Ingest(context.Background(), IngestRequest{Date: "not-a-date"}); err == nil {
			t.Fatal("expected the ingest to be refused")
		}
		if got := probe.plans(t); got != 0 {
			t.Fatalf("ailm_plans_created_total = %v, want 0 for a malformed request", got)
		}
	})
}

// --- ailm_trucks_packed_total ------------------------------------------------

func TestPackMetersTrucksThatWerePacked(t *testing.T) {
	t.Run("one truck packed and stored counts once", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-pack-ok")
		svc := newTestService(newFakePlanStore(planWithPackedLoad()),
			&fakeGable{vehicles: testVehicles()}, Config{}).WithMeter(probe.m)

		if _, err := svc.Pack(context.Background(), "plan-1"); err != nil {
			t.Fatalf("pack: %v", err)
		}
		if got := probe.trucks(t); got != 1 {
			t.Fatalf("ailm_trucks_packed_total = %v, want 1", got)
		}
	})

	t.Run("a packed plan that failed to store does not count", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-pack-store-failed")
		store := errPlanStore{inner: newFakePlanStore(planWithPackedLoad()), err: errors.New("update failed")}
		svc := newTestService(store, &fakeGable{vehicles: testVehicles()}, Config{}).WithMeter(probe.m)

		if _, err := svc.Pack(context.Background(), "plan-1"); err == nil {
			t.Fatal("expected the pack to fail")
		}
		if got := probe.trucks(t); got != 0 {
			t.Fatalf("ailm_trucks_packed_total = %v, want 0 — the solve was discarded, not delivered", got)
		}
	})

	t.Run("a refused pack does not count", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-pack-refused")
		unassigned := planWithPackedLoad()
		unassigned.Loads = nil
		svc := newTestService(newFakePlanStore(unassigned), &fakeGable{vehicles: testVehicles()}, Config{}).WithMeter(probe.m)

		if _, err := svc.Pack(context.Background(), "plan-1"); err == nil {
			t.Fatal("expected the pack to be refused — nothing is assigned")
		}
		if got := probe.trucks(t); got != 0 {
			t.Fatalf("ailm_trucks_packed_total = %v, want 0", got)
		}
	})
}

// --- ailm_routes_pushed_total ------------------------------------------------

func TestPushMetersRoutesActuallyWrittenBack(t *testing.T) {
	t.Run("one route on the dispatch board counts once", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-push-ok")
		g := &fakeGable{}
		svc := newTestService(newFakePlanStore(pushReadyPlan()), g, Config{}).WithMeter(probe.m)

		if _, err := svc.Push(context.Background(), "plan-1"); err != nil {
			t.Fatalf("push: %v", err)
		}
		if got, want := probe.routes(t), float64(len(g.pushed)); got != want {
			t.Fatalf("ailm_routes_pushed_total = %v, want %v (the routes the ERP actually took)", got, want)
		}
	})

	t.Run("a push the gate refused counts nothing", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-push-refused")
		unsigned := planWithPackedLoad() // proof attached but never signed off
		g := &fakeGable{}
		svc := newTestService(newFakePlanStore(unsigned), g, Config{}).WithMeter(probe.m)

		if _, err := svc.Push(context.Background(), "plan-1"); err == nil {
			t.Fatal("expected the depart gate to refuse an unsigned truck")
		}
		if len(g.pushed) != 0 {
			t.Fatalf("the gate let %d route(s) through", len(g.pushed))
		}
		if got := probe.routes(t); got != 0 {
			t.Fatalf("ailm_routes_pushed_total = %v, want 0", got)
		}
	})

	t.Run("a push that failed at the ERP counts nothing", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-push-erp-down")
		g := &fakeGable{pushErr: errors.New("GableLBM 500")}
		svc := newTestService(newFakePlanStore(pushReadyPlan()), g, Config{}).WithMeter(probe.m)

		if _, err := svc.Push(context.Background(), "plan-1"); err == nil {
			t.Fatal("expected the push to fail")
		}
		if got := probe.routes(t); got != 0 {
			t.Fatalf("ailm_routes_pushed_total = %v, want 0 — nothing reached the dispatch board", got)
		}
	})

	// The interesting case. A push that dies half-way has still put real routes
	// on a real dispatch board; drivers are loading against them. Counting zero
	// because the batch errored would under-report work the dealer received.
	t.Run("a partial push counts the routes that landed", func(t *testing.T) {
		probe := newMeterProbe(t, "meter-push-partial")
		p := twoTruckPushReadyPlan()
		g := &failAfterNPushes{n: 1, err: errors.New("GableLBM 503 on the second truck")}
		svc := newTestService(newFakePlanStore(p), nil, Config{}).WithMeter(probe.m)
		svc.gable = g

		if _, err := svc.Push(context.Background(), "plan-1"); err == nil {
			t.Fatal("expected the push to fail on the second truck")
		}
		if g.calls != 2 {
			t.Fatalf("expected 2 push attempts, got %d", g.calls)
		}
		if got := probe.routes(t); got != 1 {
			t.Fatalf("ailm_routes_pushed_total = %v, want 1 — the first truck's route is on the board and does not un-happen", got)
		}
	})
}

// TestServiceWithoutAMeterStillWorks is the rollback guarantee at this layer:
// every other test in this package constructs a Service with no meter, so if
// the nil path were not safe, they would all be failing. This one says so on
// purpose rather than by accident.
func TestServiceWithoutAMeterStillWorks(t *testing.T) {
	svc := newTestService(newFakePlanStore(pushReadyPlan()), &fakeGable{}, Config{})
	if _, err := svc.Push(context.Background(), "plan-1"); err != nil {
		t.Fatalf("a Service with no meter must behave exactly as before: %v", err)
	}
}

// --- doubles -----------------------------------------------------------------

// failingCreateStore fails the INSERT (errPlanStore in fakes_test.go fails the
// UPDATE, which is the other half of the same question).
type failingCreateStore struct {
	inner *fakePlanStore
	err   error
}

func (s failingCreateStore) Create(context.Context, *Plan) error { return s.err }
func (s failingCreateStore) Update(ctx context.Context, p *Plan) error {
	return s.inner.Update(ctx, p)
}
func (s failingCreateStore) Get(ctx context.Context, id string) (*Plan, error) {
	return s.inner.Get(ctx, id)
}
func (s failingCreateStore) GetLatestForDate(ctx context.Context, d string) (*Plan, error) {
	return s.inner.GetLatestForDate(ctx, d)
}

// failAfterNPushes accepts n routes and then fails, modelling an ERP that goes
// away mid-batch.
type failAfterNPushes struct {
	fakeGable
	n     int
	calls int
	err   error
}

func (f *failAfterNPushes) PushDeliveryRoute(ctx context.Context, r gable.DeliveryRoute) error {
	f.calls++
	if f.calls > f.n {
		return f.err
	}
	return f.fakeGable.PushDeliveryRoute(ctx, r)
}

// twoTruckPushReadyPlan is a plan whose two trucks both clear every depart
// gate, so a mid-batch failure is the only thing that can stop the second one.
func twoTruckPushReadyPlan() *Plan {
	p := pushReadyPlan()
	second := clonePlan(p).Loads[0]
	second.VehicleID = "v2"
	second.VehicleName = "Flatbed 2"
	p.Loads = append(p.Loads, second)
	return p
}
