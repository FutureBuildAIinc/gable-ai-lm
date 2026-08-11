// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package compliance enforces gross-vehicle-weight rules and flags restricted
// points (weight/height/width-limited bridges, overpasses) along a route. The
// MVP uses lat/lng + a haversine buffer instead of PostGIS geometry.
package compliance

import "time"

// RestrictedPoint is a geofenced restriction on the road network.
type RestrictedPoint struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Lat               float64   `json:"lat"`
	Lng               float64   `json:"lng"`
	RestrictionType   string    `json:"restriction_type"` // WEIGHT/HEIGHT/WIDTH/SEASONAL
	MaxGrossWeightLbs *int64    `json:"max_gross_weight_lbs,omitempty"`
	MaxAxleWeightLbs  *int64    `json:"max_axle_weight_lbs,omitempty"`
	MaxHeightIn       *float64  `json:"max_height_in,omitempty"`
	Notes             string    `json:"notes"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// RestrictedPointInput is the create/update payload.
type RestrictedPointInput struct {
	Name              string   `json:"name"`
	Lat               float64  `json:"lat"`
	Lng               float64  `json:"lng"`
	RestrictionType   string   `json:"restriction_type"`
	MaxGrossWeightLbs *int64   `json:"max_gross_weight_lbs,omitempty"`
	MaxAxleWeightLbs  *int64   `json:"max_axle_weight_lbs,omitempty"`
	MaxHeightIn       *float64 `json:"max_height_in,omitempty"`
	Notes             string   `json:"notes"`
}

// RoutePoint is one vertex of a planned route polyline.
type RoutePoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// LoadProfile describes the loaded vehicle being checked against restrictions.
type LoadProfile struct {
	GrossWeightLbs int64   `json:"gross_weight_lbs"`
	MaxAxleLbs     int64   `json:"max_axle_lbs"`
	HeightIn       float64 `json:"height_in"`
}

// RouteCheckRequest asks whether a load can traverse a route.
type RouteCheckRequest struct {
	Route       []RoutePoint `json:"route"`
	Load        LoadProfile  `json:"load"`
	BufferMiles float64      `json:"buffer_miles,omitempty"` // default 0.5
}

// Flag is a single restriction the load violates (or comes near).
type Flag struct {
	Point      RestrictedPoint `json:"point"`
	DistanceMi float64         `json:"distance_mi"`
	Violation  string          `json:"violation"` // human-readable reason
	Severity   string          `json:"severity"`  // WARN/FAIL
}

// RouteCheckResult is the outcome of a route compliance check.
type RouteCheckResult struct {
	Status string `json:"status"` // PASS/WARN/FAIL
	Flags  []Flag `json:"flags"`
}
