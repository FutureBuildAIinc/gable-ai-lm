// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// Exported seams for the workflow orchestrator. The workflow module reuses the
// routing heuristics (CVRP assignment + sequencing) without going through the
// HTTP-facing routing service, so the deterministic internals are re-exported
// here as thin wrappers.

// AssignLoads bin-packs stops onto vehicles by weight capacity (FFD).
func AssignLoads(vehicles []gable.Vehicle, stops []Stop) (loads []Load, unassigned []Stop) {
	return assignLoads(vehicles, stops)
}

// AssignDrivers attaches drivers to loads round-robin (ACTIVE first).
func AssignDrivers(drivers []gable.Driver, loads []Load) {
	assignDrivers(drivers, loads)
}

// OptimizeSequence orders stops from the depot and returns the sequenced stops
// with total distance (mi) and duration (min). It delegates to the active
// SequenceOptimizer (haversine by default; ORS road-matrix when configured), so
// existing callers pick up real OSS routing without changing. Callers without a
// context use a background one; the ORS provider carries its own HTTP timeout.
func OptimizeSequence(depotLat, depotLng float64, stops []Stop) ([]Stop, float64, float64) {
	return active.Sequence(context.Background(), depotLat, depotLng, stops)
}

// HaversineMiles is the great-circle distance between two coordinates.
func HaversineMiles(lat1, lng1, lat2, lng2 float64) float64 {
	return haversineMiles(lat1, lng1, lat2, lng2)
}
