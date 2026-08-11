// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package routing builds a pre-optimized daily delivery route from confirmed
// GableLBM orders. The MVP optimizer is a deterministic nearest-neighbor + 2-opt
// heuristic over haversine distances; it is pluggable for a real distance-matrix
// provider later. Approved plans are written back to GableLBM.
package routing

import "time"

// Stop is one delivery stop in a route plan.
type Stop struct {
	OrderID   string  `json:"order_id"`
	Sequence  int     `json:"sequence"`
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	Address   string  `json:"address,omitempty"`
	WeightLbs float64 `json:"weight_lbs"`
	// VolumeCuFt is the stop's total cargo bounding volume, used by the workflow
	// assignment step to cap a truck by bed space as well as by weight (T2-2).
	VolumeCuFt float64 `json:"volume_cuft,omitempty"`
}

// Load is a single truck's assignment: its vehicle, capacity, sequenced stops
// and the per-load totals. One Load becomes one delivery_route on write-back.
type Load struct {
	VehicleID         string  `json:"vehicle_id"`
	VehicleName       string  `json:"vehicle_name"`
	DriverID          string  `json:"driver_id"`
	DriverName        string  `json:"driver_name"`
	CapacityWeightLbs int     `json:"capacity_weight_lbs"`
	Stops             []Stop  `json:"stops"`
	TotalWeightLbs    float64 `json:"total_weight_lbs"`
	TotalDistanceMi   float64 `json:"total_distance_mi"`
	TotalDurationMin  float64 `json:"total_duration_min"`
}

// Plan is a cached, capacitated route plan for a date. Loads holds the per-truck
// assignments; Stops/Total* are the flattened union/sums across loads (kept for
// back-compat with the 3D/summary code). UnassignedStops are stops that did not
// fit any available vehicle.
type Plan struct {
	ID               string    `json:"id"`
	PlanDate         string    `json:"plan_date"` // YYYY-MM-DD
	GableBranchID    *string   `json:"gable_branch_id,omitempty"`
	GableVehicleID   *string   `json:"gable_vehicle_id,omitempty"`
	Loads            []Load    `json:"loads"`
	UnassignedStops  []Stop    `json:"unassigned_stops"`
	Stops            []Stop    `json:"stops"`
	TotalDistanceMi  float64   `json:"total_distance_mi"`
	TotalDurationMin float64   `json:"total_duration_min"`
	Status           string    `json:"status"` // DRAFT/APPROVED
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// PlanRequest asks the optimizer to build a draft plan for a date.
type PlanRequest struct {
	Date      string   `json:"date"` // YYYY-MM-DD
	BranchID  *string  `json:"branch_id,omitempty"`
	VehicleID *string  `json:"vehicle_id,omitempty"`
	DepotLat  *float64 `json:"depot_lat,omitempty"`
	DepotLng  *float64 `json:"depot_lng,omitempty"`
}
