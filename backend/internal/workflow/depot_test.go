// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"math"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// The depot the module used to hardcode — one specific dealer's yard in
// Kelowna BC. No code path may ever produce it again for a dealer that did not
// ask for it: every route, distance, duration and shift check is rooted there.
const (
	retiredGableDepotLat = 49.8863
	retiredGableDepotLng = -119.4666
)

func texasOrders() []gable.Order {
	return []gable.Order{
		{ID: "o1", CustomerName: "Lone Star Framing", Latitude: fptr(32.70), Longitude: fptr(-96.90),
			Lines: []gable.OrderLine{{ProductID: "p1", SKU: "2x4-8", Quantity: 10, WeightLbs: 10}}},
		{ID: "o2", CustomerName: "Bluebonnet Homes", Latitude: fptr(32.90), Longitude: fptr(-97.10),
			Lines: []gable.OrderLine{{ProductID: "p1", SKU: "2x4-8", Quantity: 10, WeightLbs: 10}}},
	}
}

// TestResolveDepotPrecedence pins the three-level precedence: the ingest
// request wins, then this install's configured yard, then the centroid of the
// day's own stops. There is no built-in coordinate at any level.
func TestResolveDepotPrecedence(t *testing.T) {
	analyses := []OrderAnalysis{
		{OrderID: "a", Lat: fptr(32.70), Lng: fptr(-96.90), Routable: true},
		{OrderID: "b", Lat: fptr(32.90), Lng: fptr(-97.10), Routable: true},
		{OrderID: "c", Lat: nil, Lng: nil, Routable: false}, // must not skew the centroid
	}
	cfgDepot := Config{DepotLat: fptr(30.25), DepotLng: fptr(-97.75)}

	cases := []struct {
		name             string
		req              IngestRequest
		cfg              Config
		analyses         []OrderAnalysis
		wantLat, wantLng float64
		wantSource       string
	}{
		{
			name: "request depot wins over config and stops",
			req:  IngestRequest{Date: "2026-06-26", DepotLat: fptr(29.76), DepotLng: fptr(-95.36)},
			cfg:  cfgDepot, analyses: analyses,
			wantLat: 29.76, wantLng: -95.36, wantSource: DepotSourceRequest,
		},
		{
			name: "configured depot wins over the centroid",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  cfgDepot, analyses: analyses,
			wantLat: 30.25, wantLng: -97.75, wantSource: DepotSourceConfig,
		},
		{
			name: "centroid of routable stops when nothing is configured",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  Config{}, analyses: analyses,
			wantLat: 32.80, wantLng: -97.00, wantSource: DepotSourceCentroid,
		},
		{
			name: "half-configured depot is ignored, not half-applied",
			req:  IngestRequest{Date: "2026-06-26", DepotLat: fptr(29.76)},
			cfg:  Config{DepotLat: fptr(30.25)}, analyses: analyses,
			wantLat: 32.80, wantLng: -97.00, wantSource: DepotSourceCentroid,
		},
		{
			name: "nothing to root on",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  Config{}, analyses: []OrderAnalysis{{OrderID: "z", Routable: false}},
			wantLat: 0, wantLng: 0, wantSource: DepotSourceNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lat, lng, src := resolveDepot(tc.req, tc.cfg, tc.analyses)
			if math.Abs(lat-tc.wantLat) > 1e-6 || math.Abs(lng-tc.wantLng) > 1e-6 {
				t.Fatalf("depot = (%v, %v), want (%v, %v)", lat, lng, tc.wantLat, tc.wantLng)
			}
			if src != tc.wantSource {
				t.Fatalf("depot source = %q, want %q", src, tc.wantSource)
			}
		})
	}
}

// TestIngestUsesConfiguredDepot verifies the configured yard reaches the
// persisted plan (the dealer's own coordinates, not a code constant).
func TestIngestUsesConfiguredDepot(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{orders: texasOrders()},
		Config{DepotLat: fptr(32.7767), DepotLng: fptr(-96.7970)})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotLat != 32.7767 || p.DepotLng != -96.7970 {
		t.Fatalf("plan depot = (%v, %v), want the configured yard (32.7767, -96.7970)", p.DepotLat, p.DepotLng)
	}
	if p.DepotSource != DepotSourceConfig {
		t.Fatalf("depot source = %q, want %q", p.DepotSource, DepotSourceConfig)
	}
	if got := store.stored(p.ID); got.DepotLat != p.DepotLat || got.DepotSource != DepotSourceConfig {
		t.Fatalf("depot did not persist: %+v", got)
	}
}

// TestIngestUnconfiguredDepotUsesStopCentroidNotAForeignYard is the regression
// guard for the hardcoded-depot blocker: an unconfigured install must root a
// Texas dealer's plan in Texas — near its own stops — never at the Kelowna BC
// yard the module used to hardcode ~1,700 miles away.
func TestIngestUnconfiguredDepotUsesStopCentroidNotAForeignYard(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{orders: texasOrders()}, Config{})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotLat == retiredGableDepotLat || p.DepotLng == retiredGableDepotLng {
		t.Fatalf("plan rooted at the retired hardcoded Kelowna depot (%v, %v)", p.DepotLat, p.DepotLng)
	}
	if p.DepotSource != DepotSourceCentroid {
		t.Fatalf("depot source = %q, want %q", p.DepotSource, DepotSourceCentroid)
	}
	// Centroid of the two Texas stops.
	if math.Abs(p.DepotLat-32.80) > 1e-6 || math.Abs(p.DepotLng-(-97.00)) > 1e-6 {
		t.Fatalf("depot = (%v, %v), want the stop centroid (32.80, -97.00)", p.DepotLat, p.DepotLng)
	}
}

// TestIngestRequestDepotOverridesConfig verifies a per-run override still wins.
func TestIngestRequestDepotOverridesConfig(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{orders: texasOrders()},
		Config{DepotLat: fptr(32.7767), DepotLng: fptr(-96.7970)})

	p, err := svc.Ingest(context.Background(), IngestRequest{
		Date: "2026-06-26", DepotLat: fptr(29.7604), DepotLng: fptr(-95.3698),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotLat != 29.7604 || p.DepotLng != -95.3698 || p.DepotSource != DepotSourceRequest {
		t.Fatalf("request depot must win: got (%v, %v) source %q", p.DepotLat, p.DepotLng, p.DepotSource)
	}
}
