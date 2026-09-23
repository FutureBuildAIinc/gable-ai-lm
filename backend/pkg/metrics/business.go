// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Business (metering) metrics.
//
// Until now pkg/metrics counted HTTP requests and database connections — how
// hard the process is working, and nothing about what it is FOR. These four
// count the events that correlate with value: a plan was created, a truck was
// packed, a route was written back to the ERP, the catalog was pulled. They are
// what makes a managed deployment meterable and a self-hosted one auditable.
//
// # Labels
//
// Every series carries the deployment's licence identity: `edition` and
// `subject`, taken from internal/license. With one stack per dealer the
// deployment IS the tenant, so metering needs no tenancy model, no per-request
// resolution, and no schema change — the token carries the identity and the
// labels carry the token. `subject` is "unlicensed" (never empty) when no token
// is present, because an empty label value is indistinguishable from a bug.
//
// The label cardinality is 1 per process, by construction: a running instance
// has exactly one licence. If a future version ever resolves a subject
// per-request, this comment stops being true and the cardinality stops being
// bounded — do not do that without changing the design deliberately.
//
// # Scrape-only
//
// There is no outbound usage feed. v1 is deliberately scrape-only: an
// unsolicited phone-home from a self-hosted instance is a trust problem, and
// where usage should go is a business decision that has not been made. These
// are exposed on the existing /metrics endpoint and nowhere else.
var (
	PlansCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ailm_plans_created_total",
			Help: "Dispatch plans created (ingest + analyze), by license edition and subject.",
		},
		[]string{"edition", "subject"},
	)

	TrucksPackedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ailm_trucks_packed_total",
			Help: "Truck loads 3D-packed, by license edition and subject.",
		},
		[]string{"edition", "subject"},
	)

	RoutesPushedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ailm_routes_pushed_total",
			Help: "Delivery routes written back to the ERP, by license edition and subject.",
		},
		[]string{"edition", "subject"},
	)

	CatalogPullsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ailm_catalog_pulls_total",
			Help: "Catalog pulls from the ERP's PIM, by license edition and subject.",
		},
		[]string{"edition", "subject"},
	)
)

// Meter records business events under one deployment's licence identity.
//
// It is constructed once at boot from the loaded licence and injected into the
// services that own the events. Passing a value rather than reaching for a
// global is what lets a test assert a count without racing every other test in
// the package, and it is why the labels cannot drift away from the licence the
// boot line reported.
//
// EVERY METHOD IS NIL-SAFE. A nil *Meter records nothing and panics at nothing:
// the seam is a pure addition, so a service constructed without one (every
// existing unit test, and any caller written before this landed) behaves
// exactly as it did before. That is also the rollback story — reverting the
// wiring leaves working code, not nil dereferences.
type Meter struct {
	plans   prometheus.Counter
	trucks  prometheus.Counter
	routes  prometheus.Counter
	catalog prometheus.Counter
}

// NewMeter binds the four counters to this deployment's labels.
//
// It touches each series immediately, so all four appear on /metrics at 0 from
// the first scrape. A counter that springs into existence on its first
// increment is a counter that reads as "no data" and "zero" identically, and a
// dashboard cannot tell a quiet dealer from a broken exporter.
func NewMeter(edition, subject string) *Meter {
	if subject == "" {
		subject = "unlicensed"
	}
	labels := prometheus.Labels{"edition": edition, "subject": subject}
	return &Meter{
		plans:   PlansCreatedTotal.With(labels),
		trucks:  TrucksPackedTotal.With(labels),
		routes:  RoutesPushedTotal.With(labels),
		catalog: CatalogPullsTotal.With(labels),
	}
}

// PlanCreated records one dispatch plan. Call it AFTER the plan is persisted:
// a plan that failed to store is not a plan, and metering it would bill for
// work that does not exist.
func (m *Meter) PlanCreated() {
	if m == nil {
		return
	}
	m.plans.Inc()
}

// TrucksPacked records n truck loads solved. Call it AFTER the packed plan is
// persisted, and with the number actually packed — not the number attempted.
func (m *Meter) TrucksPacked(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.trucks.Add(float64(n))
}

// RoutesPushed records n routes written back to the ERP. Call it with the
// number the ERP actually accepted: a push that fails part-way through has
// delivered the routes it got through, and no more.
func (m *Meter) RoutesPushed(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.routes.Add(float64(n))
}

// CatalogPulled records one successful pull of the PIM catalog. Call it only
// when the ERP answered: a failed pull produced no catalog.
func (m *Meter) CatalogPulled() {
	if m == nil {
		return
	}
	m.catalog.Inc()
}
