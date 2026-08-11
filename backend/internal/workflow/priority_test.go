// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/routing"
)

func priStopIDs(stops []routing.Stop) []string {
	out := make([]string, len(stops))
	for i, s := range stops {
		out[i] = s.OrderID
	}
	return out
}

// TestSequenceWithPriorityPinsToFront verifies a priority stop is sequenced
// first even when it is the farthest from the depot (haversine would order it
// last), and that sequence numbers stay contiguous 1..n.
func TestSequenceWithPriorityPinsToFront(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0
	stops := []routing.Stop{
		{OrderID: "near", Lat: 49.01, Lng: -119.01, WeightLbs: 100},
		{OrderID: "far", Lat: 49.40, Lng: -119.50, WeightLbs: 100}, // farthest, but priority
		{OrderID: "mid", Lat: 49.08, Lng: -119.08, WeightLbs: 100},
	}

	ordered, dist, dur := sequenceWithPriority(depotLat, depotLng, stops, map[string]bool{"far": true})

	if len(ordered) != 3 {
		t.Fatalf("expected 3 stops, got %d", len(ordered))
	}
	if ordered[0].OrderID != "far" {
		t.Fatalf("priority stop not pinned first: got %v", priStopIDs(ordered))
	}
	for i, s := range ordered {
		if s.Sequence != i+1 {
			t.Fatalf("sequence not contiguous: position %d has sequence %d", i, s.Sequence)
		}
	}
	if dist <= 0 || dur <= 0 {
		t.Fatalf("expected positive totals, got dist=%.2f dur=%.2f", dist, dur)
	}
}

// TestSequenceWithPriorityMultiplePinnedFirst verifies that when several stops
// are priority, they all precede every non-priority stop.
func TestSequenceWithPriorityMultiplePinnedFirst(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0
	stops := []routing.Stop{
		{OrderID: "p1", Lat: 49.30, Lng: -119.30},
		{OrderID: "n1", Lat: 49.02, Lng: -119.02},
		{OrderID: "p2", Lat: 49.25, Lng: -119.20},
		{OrderID: "n2", Lat: 49.05, Lng: -119.05},
	}
	pri := map[string]bool{"p1": true, "p2": true}

	ordered, _, _ := sequenceWithPriority(depotLat, depotLng, stops, pri)

	// First two must be the priority stops (in some optimized order).
	frontPriority := pri[ordered[0].OrderID] && pri[ordered[1].OrderID]
	backPlain := !pri[ordered[2].OrderID] && !pri[ordered[3].OrderID]
	if !frontPriority || !backPlain {
		t.Fatalf("priority stops not all pinned to front: %v", priStopIDs(ordered))
	}
}

// TestSequenceWithPriorityNoneMatchesDefault verifies that with no priority
// stops the result equals the plain depot-rooted optimization.
func TestSequenceWithPriorityNoneMatchesDefault(t *testing.T) {
	depotLat, depotLng := 49.0, -119.0
	stops := []routing.Stop{
		{OrderID: "a", Lat: 49.10, Lng: -119.10},
		{OrderID: "b", Lat: 49.30, Lng: -119.40},
		{OrderID: "c", Lat: 49.05, Lng: -119.05},
	}

	got, gotDist, gotDur := sequenceWithPriority(depotLat, depotLng, stops, map[string]bool{})
	want, wantDist, wantDur := routing.OptimizeSequence(depotLat, depotLng, stops)

	if a, b := priStopIDs(got), priStopIDs(want); len(a) != len(b) {
		t.Fatalf("length mismatch")
	} else {
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("order mismatch without priority: got %v want %v", a, b)
			}
		}
	}
	if gotDist != wantDist || gotDur != wantDur {
		t.Fatalf("totals mismatch: got (%.2f,%.2f) want (%.2f,%.2f)", gotDist, gotDur, wantDist, wantDur)
	}
}

// TestSequenceWithPriorityEmpty verifies the empty-input contract.
func TestSequenceWithPriorityEmpty(t *testing.T) {
	ordered, dist, dur := sequenceWithPriority(49.0, -119.0, nil, map[string]bool{"x": true})
	if len(ordered) != 0 || dist != 0 || dur != 0 {
		t.Fatalf("expected empty result, got %d stops dist=%.2f dur=%.2f", len(ordered), dist, dur)
	}
}
