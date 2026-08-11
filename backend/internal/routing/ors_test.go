// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sampleStops() []Stop {
	return []Stop{
		{OrderID: "a", Lat: 49.10, Lng: -119.10, WeightLbs: 1000},
		{OrderID: "b", Lat: 49.30, Lng: -119.40, WeightLbs: 1000},
		{OrderID: "c", Lat: 49.05, Lng: -119.05, WeightLbs: 1000},
	}
}

func orderIDs(stops []Stop) []string {
	out := make([]string, len(stops))
	for i, s := range stops {
		out[i] = s.OrderID
	}
	return out
}

// TestORSProviderFallbackNoKey verifies that with no API key the provider
// degrades to the exact haversine optimization (never hard-fails).
func TestORSProviderFallbackNoKey(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0
	p := NewORSProvider("", "", "")
	if p.Configured() {
		t.Fatal("provider with empty key should report not configured")
	}

	got, gotDist, gotDur := p.Sequence(context.Background(), depotLat, depotLng, sampleStops())
	want, wantDist, wantDur := OptimizeSequence(depotLat, depotLng, sampleStops())

	if a, b := orderIDs(got), orderIDs(want); !equalStrings(a, b) {
		t.Fatalf("fallback order mismatch: got %v want %v", a, b)
	}
	if gotDist != wantDist || gotDur != wantDur {
		t.Fatalf("fallback totals mismatch: got (%.2f,%.2f) want (%.2f,%.2f)", gotDist, gotDur, wantDist, wantDur)
	}
}

// TestORSProviderFallbackOnError verifies that an ORS error (non-200 here) is
// swallowed and the haversine optimizer is used instead.
func TestORSProviderFallbackOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"quota exceeded"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	depotLat, depotLng := 49.0, -119.0
	p := NewORSProvider("test-key", srv.URL, "driving-hgv")
	got, _, _ := p.Sequence(context.Background(), depotLat, depotLng, sampleStops())
	want, _, _ := OptimizeSequence(depotLat, depotLng, sampleStops())

	if a, b := orderIDs(got), orderIDs(want); !equalStrings(a, b) {
		t.Fatalf("error fallback order mismatch: got %v want %v", a, b)
	}
}

// TestORSProviderMatrixSequencing verifies a happy-path matrix call: the
// returned order follows the road matrix (not haversine), the request uses
// [lng,lat] ordering + the driving-hgv profile + raw-key Authorization, and the
// totals come from the matrix (distance in mi, duration sec→min).
func TestORSProviderMatrixSequencing(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0

	// Matrix index 0 = depot; stop k = index k+1. Distances (mi).
	dist := [][]float64{
		{0, 10, 20, 1}, // depot
		{10, 0, 2, 5},  // stop a (idx1)
		{20, 2, 0, 8},  // stop b (idx2)
		{1, 5, 8, 0},   // stop c (idx3)
	}
	dur := make([][]float64, 4)
	for i := range dist {
		dur[i] = make([]float64, 4)
		for j := range dist[i] {
			dur[i][j] = dist[i][j] * 60 // seconds
		}
	}

	var gotProfile, gotAuth string
	var gotLocations [][2]float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProfile = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body orsMatrixRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotLocations = body.Locations
		_ = json.NewEncoder(w).Encode(orsMatrixResponse{Distances: ptrMatrix(dist), Durations: ptrMatrix(dur)})
	}))
	defer srv.Close()

	p := NewORSProvider("secret-key", srv.URL, "driving-hgv")
	got, gotDist, gotDur := p.Sequence(context.Background(), depotLat, depotLng, sampleStops())

	// NN from depot: c(1) → a(5) → b(2): expected order c, a, b.
	if want := []string{"c", "a", "b"}; !equalStrings(orderIDs(got), want) {
		t.Fatalf("matrix order mismatch: got %v want %v", orderIDs(got), want)
	}
	for i, s := range got {
		if s.Sequence != i+1 {
			t.Fatalf("sequence not contiguous: stop %d has sequence %d", i, s.Sequence)
		}
	}
	// distance = 1 + 5 + 2 = 8 mi; duration = 480 sec / 60 = 8 min.
	if gotDist != 8 || gotDur != 8 {
		t.Fatalf("matrix totals mismatch: got (%.2f mi, %.2f min) want (8, 8)", gotDist, gotDur)
	}

	if gotProfile != "/v2/matrix/driving-hgv" {
		t.Errorf("unexpected matrix path: %s", gotProfile)
	}
	if gotAuth != "secret-key" {
		t.Errorf("ORS POST should send raw key in Authorization, got %q", gotAuth)
	}
	if len(gotLocations) != 4 {
		t.Fatalf("expected 4 locations (depot + 3 stops), got %d", len(gotLocations))
	}
	// [lng,lat] ordering: depot location must be [depotLng, depotLat].
	if gotLocations[0][0] != depotLng || gotLocations[0][1] != depotLat {
		t.Errorf("depot coord not [lng,lat]: got %v want [%v,%v]", gotLocations[0], depotLng, depotLat)
	}
}

// ptrMatrix lifts a dense matrix into the *float64 wire shape ORS actually
// uses (a cell is a pointer so JSON null — "no route" — stays distinguishable
// from a real 0).
func ptrMatrix(m [][]float64) [][]*float64 {
	out := make([][]*float64, len(m))
	for i := range m {
		out[i] = make([]*float64, len(m[i]))
		for j := range m[i] {
			v := m[i][j]
			out[i][j] = &v
		}
	}
	return out
}

// TestSolidifyMatrixRejectsUnroutablePairs pins the cell-level contract: a
// null / non-finite / negative cost, or a phantom zero-cost leg between two
// distinct coordinates, is an error — while a genuine 0 between two orders at
// the SAME geocoded jobsite is accepted.
func TestSolidifyMatrixRejectsUnroutablePairs(t *testing.T) {
	locs := [][2]float64{{-119.0, 49.0}, {-119.1, 49.1}, {-119.2, 49.2}}
	f := func(v float64) *float64 { return &v }
	ok := [][]*float64{
		{f(0), f(5), f(9)},
		{f(5), f(0), f(4)},
		{f(9), f(4), f(0)},
	}
	if _, err := solidifyMatrix("distance", ok, locs); err != nil {
		t.Fatalf("a well-formed matrix must be accepted, got %v", err)
	}

	cases := map[string][][]*float64{
		"null cell": {
			{f(0), nil, f(9)},
			{f(5), f(0), f(4)},
			{f(9), f(4), f(0)},
		},
		"negative cell": {
			{f(0), f(-5), f(9)},
			{f(5), f(0), f(4)},
			{f(9), f(4), f(0)},
		},
		"non-finite cell": {
			{f(0), f(math.Inf(1)), f(9)},
			{f(5), f(0), f(4)},
			{f(9), f(4), f(0)},
		},
		"phantom zero between distinct coords": {
			{f(0), f(0), f(9)},
			{f(5), f(0), f(4)},
			{f(9), f(4), f(0)},
		},
		"short row": {
			{f(0), f(5)},
			{f(5), f(0), f(4)},
			{f(9), f(4), f(0)},
		},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := solidifyMatrix("distance", m, locs); err == nil {
				t.Fatalf("%s must be rejected so the caller falls back to haversine", name)
			}
		})
	}

	// Two orders delivered to the SAME jobsite: a 0 leg between them is real
	// and must not force a fallback.
	coLocated := [][2]float64{{-119.0, 49.0}, {-119.1, 49.1}, {-119.1, 49.1}}
	same := [][]*float64{
		{f(0), f(5), f(5)},
		{f(5), f(0), f(0)},
		{f(5), f(0), f(0)},
	}
	if _, err := solidifyMatrix("distance", same, coLocated); err != nil {
		t.Fatalf("co-located stops legitimately cost 0 to travel between, got %v", err)
	}
}

// TestORSProviderFallbackOnNullMatrix is the regression guard for the
// data-integrity bug: ORS returns JSON null for an unroutable pair, which a
// [][]float64 decode turns into 0.0 — the cheapest edge — pulling that stop to
// the front of the route and under-reporting the totals that feed the
// 480-minute shift check. The provider must reject the matrix and fall back to
// haversine instead of emitting a plausible-but-wrong route.
func TestORSProviderFallbackOnNullMatrix(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0

	// depot→b (index 0→2) is null: ORS could not route to stop "b". Every other
	// cell is a normal cost, so the shape checks alone would happily accept it.
	const nullMatrix = `{
		"distances": [
			[0, 10, null, 30],
			[10, 0, 2, 5],
			[20, 2, 0, 8],
			[30, 5, 8, 0]
		],
		"durations": [
			[0, 600, 1200, 1800],
			[600, 0, 120, 300],
			[1200, 120, 0, 480],
			[1800, 300, 480, 0]
		]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nullMatrix))
	}))
	defer srv.Close()

	p := NewORSProvider("secret-key", srv.URL, "driving-hgv")
	got, gotDist, gotDur := p.Sequence(context.Background(), depotLat, depotLng, sampleStops())
	want, wantDist, wantDur := OptimizeSequence(depotLat, depotLng, sampleStops())

	// The buggy decode makes the null leg cost 0, so nearest-neighbor visits
	// "b" first. The haversine fallback visits the genuinely nearest stop.
	if got[0].OrderID == "b" {
		t.Fatalf("unroutable stop was pulled to the front of the route: %v", orderIDs(got))
	}
	if a, b := orderIDs(got), orderIDs(want); !equalStrings(a, b) {
		t.Fatalf("null matrix must fall back to haversine: got %v want %v", a, b)
	}
	if gotDist != wantDist || gotDur != wantDur {
		t.Fatalf("null matrix totals must come from the fallback: got (%.2f,%.2f) want (%.2f,%.2f)",
			gotDist, gotDur, wantDist, wantDur)
	}
	if gotDist <= 0 || gotDur <= 0 {
		t.Fatalf("fallback totals must be real, got dist=%.2f dur=%.2f", gotDist, gotDur)
	}
}

// TestORSProviderFallbackOnNullDuration verifies the duration matrix is held to
// the same standard as the distance matrix: a null duration under-reports the
// shift-feasibility numbers even when every distance is present.
func TestORSProviderFallbackOnNullDuration(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0
	const body = `{
		"distances": [
			[0, 10, 20, 1],
			[10, 0, 2, 5],
			[20, 2, 0, 8],
			[1, 5, 8, 0]
		],
		"durations": [
			[0, 600, 1200, 60],
			[600, 0, 120, 300],
			[1200, 120, 0, 480],
			[60, 300, null, 0]
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewORSProvider("secret-key", srv.URL, "driving-hgv")
	_, gotDist, gotDur := p.Sequence(context.Background(), depotLat, depotLng, sampleStops())
	_, wantDist, wantDur := OptimizeSequence(depotLat, depotLng, sampleStops())
	if gotDist != wantDist || gotDur != wantDur {
		t.Fatalf("a null duration must fall back to haversine: got (%.2f,%.2f) want (%.2f,%.2f)",
			gotDist, gotDur, wantDist, wantDur)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
