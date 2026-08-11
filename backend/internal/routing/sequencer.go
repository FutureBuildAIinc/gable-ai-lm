// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import "context"

// SequenceOptimizer orders a truck's stops from the depot and returns the
// sequenced stops with total travel distance (mi) and duration (min). It is the
// swappable routing seam: the default is the deterministic haversine
// nearest-neighbor + 2-opt heuristic; an OpenRouteService-backed provider
// (real road distance/duration matrix, driving-hgv) plugs in behind the same
// interface so callers — the routing service and the workflow orchestrator —
// never change.
type SequenceOptimizer interface {
	Sequence(ctx context.Context, depotLat, depotLng float64, stops []Stop) ([]Stop, float64, float64)
	// Name identifies the active provider for boot logging / diagnostics.
	Name() string
}

// haversineOptimizer is the default great-circle heuristic (no external maps
// provider). It wraps the existing optimizeSequence so its behavior — and its
// tests — are unchanged.
type haversineOptimizer struct{}

func (haversineOptimizer) Sequence(_ context.Context, depotLat, depotLng float64, stops []Stop) ([]Stop, float64, float64) {
	return optimizeSequence(depotLat, depotLng, stops)
}

func (haversineOptimizer) Name() string { return "haversine" }

// active is the process-wide optimizer. It defaults to haversine so the service
// runs with no routing keys; main.go swaps in the ORS provider when configured.
var active SequenceOptimizer = haversineOptimizer{}

// SetOptimizer installs the active routing optimizer (called once at startup).
// A nil value is ignored so the haversine default always remains.
func SetOptimizer(o SequenceOptimizer) {
	if o != nil {
		active = o
	}
}

// ActiveOptimizerName reports which optimizer is currently installed.
func ActiveOptimizerName() string { return active.Name() }
