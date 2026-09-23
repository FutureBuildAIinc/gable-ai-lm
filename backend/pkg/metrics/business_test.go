// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestTheFourCountersExistWithTheAgreedNames pins the metric names and label
// set. These are a published interface: a dashboard, an alert and (one day) a
// billing query all key on these exact strings, and renaming one silently is
// how a counter stops being scraped without anybody noticing.
func TestTheFourCountersExistWithTheAgreedNames(t *testing.T) {
	want := map[string]*prometheus.CounterVec{
		"ailm_plans_created_total": PlansCreatedTotal,
		"ailm_trucks_packed_total": TrucksPackedTotal,
		"ailm_routes_pushed_total": RoutesPushedTotal,
		"ailm_catalog_pulls_total": CatalogPullsTotal,
	}
	for name, vec := range want {
		reg := prometheus.NewPedanticRegistry()
		if err := reg.Register(vec); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		// Materialise one series so the family is gatherable.
		vec.WithLabelValues("evaluation", "unlicensed").Add(0)

		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather %s: %v", name, err)
		}
		if len(families) != 1 {
			t.Fatalf("%s: gathered %d families, want 1", name, len(families))
		}
		got := families[0]
		if got.GetName() != name {
			t.Errorf("metric name = %q, want %q", got.GetName(), name)
		}
		if got.GetType() != dto.MetricType_COUNTER {
			t.Errorf("%s is a %v, want a counter", name, got.GetType())
		}
		labels := map[string]bool{}
		for _, lp := range got.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = true
		}
		if !labels["edition"] || !labels["subject"] || len(labels) != 2 {
			t.Errorf("%s labels = %v, want exactly edition + subject", name, labels)
		}
	}
}

// TestRegisterExposesTheBusinessCounters: an unregistered counter is not on
// /metrics, and a metering seam nobody can scrape is not a metering seam.
func TestRegisterExposesTheBusinessCounters(t *testing.T) {
	Register()

	// A CounterVec with no child yet is registered but gathers nothing, so
	// touch one series of each vec first. This is about what Register() wired
	// up, not about what has been counted.
	HTTPRequestsTotal.WithLabelValues("GET", "/probe", "200").Add(0)
	for _, vec := range []*prometheus.CounterVec{
		PlansCreatedTotal, TrucksPackedTotal, RoutesPushedTotal, CatalogPullsTotal,
	} {
		vec.WithLabelValues("evaluation", "test-register").Add(0)
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, name := range []string{
		"ailm_plans_created_total",
		"ailm_trucks_packed_total",
		"ailm_routes_pushed_total",
		"ailm_catalog_pulls_total",
	} {
		if !seen[name] {
			t.Errorf("%s is not on the default registry after Register(); it would never reach /metrics", name)
		}
	}
	// The existing series must survive the addition.
	for _, name := range []string{"http_requests_total", "db_pool_max_conns"} {
		if !seen[name] {
			t.Errorf("Register() dropped the pre-existing series %s", name)
		}
	}
}

// TestNewMeterMaterialisesEverySeriesAtZero. The operator walkthrough greps
// /metrics for ailm_ on a freshly-started, idle instance and expects four
// counters at 0. A counter that only appears on its first increment reads as
// "no data" and "zero" identically, and a dashboard cannot tell a quiet dealer
// from a broken exporter.
func TestNewMeterMaterialisesEverySeriesAtZero(t *testing.T) {
	const subject = "test-materialise"
	_ = NewMeter("evaluation", subject)

	for name, vec := range map[string]*prometheus.CounterVec{
		"ailm_plans_created_total": PlansCreatedTotal,
		"ailm_trucks_packed_total": TrucksPackedTotal,
		"ailm_routes_pushed_total": RoutesPushedTotal,
		"ailm_catalog_pulls_total": CatalogPullsTotal,
	} {
		if got := counterValue(t, vec, "evaluation", subject); got != 0 {
			t.Errorf("%s = %v on a fresh meter, want 0", name, got)
		}
		if !seriesExists(t, vec, "evaluation", subject) {
			t.Errorf("%s has no series for a fresh meter; it would be absent from the first scrape", name)
		}
	}
}

func TestMeterCountsUnderItsOwnLabels(t *testing.T) {
	const (
		edition = "community"
		subject = "test-counts"
	)
	m := NewMeter(edition, subject)

	m.PlanCreated()
	m.PlanCreated()
	m.TrucksPacked(3)
	m.RoutesPushed(1)
	m.RoutesPushed(1)
	m.RoutesPushed(1)
	m.RoutesPushed(1)
	m.CatalogPulled()

	checks := []struct {
		name string
		vec  *prometheus.CounterVec
		want float64
	}{
		{"ailm_plans_created_total", PlansCreatedTotal, 2},
		{"ailm_trucks_packed_total", TrucksPackedTotal, 3},
		{"ailm_routes_pushed_total", RoutesPushedTotal, 4},
		{"ailm_catalog_pulls_total", CatalogPullsTotal, 1},
	}
	for _, c := range checks {
		if got := counterValue(t, c.vec, edition, subject); got != c.want {
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}

	// A second deployment identity must not share a series with the first.
	other := NewMeter("commercial", "test-counts-other")
	other.PlanCreated()
	if got := counterValue(t, PlansCreatedTotal, edition, subject); got != 2 {
		t.Errorf("another meter's increment leaked onto these labels: %v", got)
	}
}

// TestNonPositiveCountsAreNotRecorded: "packed zero trucks" and "pushed minus
// one route" are not events. Recording them would make the counters lie in the
// only direction a counter can.
func TestNonPositiveCountsAreNotRecorded(t *testing.T) {
	const subject = "test-nonpositive"
	m := NewMeter("evaluation", subject)
	m.TrucksPacked(0)
	m.TrucksPacked(-4)
	m.RoutesPushed(0)
	m.RoutesPushed(-1)

	if got := counterValue(t, TrucksPackedTotal, "evaluation", subject); got != 0 {
		t.Errorf("ailm_trucks_packed_total = %v after zero/negative calls, want 0", got)
	}
	if got := counterValue(t, RoutesPushedTotal, "evaluation", subject); got != 0 {
		t.Errorf("ailm_routes_pushed_total = %v after zero/negative calls, want 0", got)
	}
}

// TestNilMeterIsSafe is the rollback guarantee. The seam is a pure addition:
// every service built before it existed, and every unit test that does not care
// about metering, holds a nil *Meter and must behave exactly as it did before.
func TestNilMeterIsSafe(t *testing.T) {
	var m *Meter
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a nil *Meter must record nothing and panic at nothing, got: %v", r)
		}
	}()
	m.PlanCreated()
	m.TrucksPacked(5)
	m.RoutesPushed(5)
	m.CatalogPulled()
}

// TestEmptySubjectBecomesUnlicensed: an empty Prometheus label value is
// indistinguishable from a bug, so the unlicensed state says so in words.
func TestEmptySubjectBecomesUnlicensed(t *testing.T) {
	NewMeter("evaluation", "").PlanCreated()
	if got := counterValue(t, PlansCreatedTotal, "evaluation", "unlicensed"); got < 1 {
		t.Errorf(`an empty subject must be recorded as "unlicensed", got %v on that series`, got)
	}
}

// --- helpers -----------------------------------------------------------------

func counterValue(t *testing.T, vec *prometheus.CounterVec, edition, subject string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(edition, subject)
	if err != nil {
		t.Fatalf("get series {edition=%q,subject=%q}: %v", edition, subject, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// seriesExists reports whether the labelled child is present in the vec's own
// collection — i.e. whether a scrape right now would include it.
func seriesExists(t *testing.T, vec *prometheus.CounterVec, edition, subject string) bool {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(vec); err != nil {
		// Already registered elsewhere in this process; gather from the vec directly.
		ch := make(chan prometheus.Metric, 128)
		go func() { vec.Collect(ch); close(ch) }()
		for m := range ch {
			var d dto.Metric
			if err := m.Write(&d); err != nil {
				t.Fatalf("write metric: %v", err)
			}
			if labelsMatch(&d, edition, subject) {
				return true
			}
		}
		return false
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			if labelsMatch(m, edition, subject) {
				return true
			}
		}
	}
	return false
}

func labelsMatch(m *dto.Metric, edition, subject string) bool {
	var gotEdition, gotSubject string
	for _, lp := range m.GetLabel() {
		switch lp.GetName() {
		case "edition":
			gotEdition = lp.GetValue()
		case "subject":
			gotSubject = lp.GetValue()
		}
	}
	return strings.EqualFold(gotEdition, edition) && gotSubject == subject
}
