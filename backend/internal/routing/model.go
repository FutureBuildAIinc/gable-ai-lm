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
	ID             string  `json:"id"`
	PlanDate       string  `json:"plan_date"` // YYYY-MM-DD
	GableBranchID  *string `json:"gable_branch_id,omitempty"`
	GableVehicleID *string `json:"gable_vehicle_id,omitempty"`

	// DepotSource and DepotNote are this plan's routing provenance, produced by
	// the shared ladder in internal/depot at the moment the origin is resolved.
	// DepotSource names where the origin came from — REQUEST / BRANCH / CONFIG /
	// CENTROID / NONE — and always names what actually happened, never what was
	// asked for. DepotNote is the operator-facing sentence explaining why the
	// branch rung was declined and where the run was rooted instead; it is empty
	// when nothing was declined, so a note is always a real warning and never
	// happy-path noise. Both are byte-identical to the values workflow.Plan
	// carries for the same run, because both come from the same package.
	//
	// They are RESPONSE-ONLY, and that is deliberate rather than an oversight.
	// route_plans is a column-backed table (migrations/001_ai_lm_core.sql) with
	// no depot_source or depot_note column, and adding one is a migration. So:
	//
	//   - POST /api/v1/routing/plan returns them POPULATED. Service.Plan fills
	//     them in as it resolves the origin, which is the moment the dispatcher
	//     is looking at a fresh route and can still act on the warning. That is
	//     the moment the sentence was written for.
	//   - GET /api/v1/routing/plan/{id} returns them EMPTY, because
	//     Repository.Get can only select columns that exist. On a re-read an
	//     empty DepotNote means "this plan's provenance was never stored" — it
	//     does NOT mean "no fallback happened". The two are indistinguishable on
	//     the read path, so nothing downstream may treat an empty note there as
	//     an all-clear.
	//
	// Never fill these in on the read path. A re-resolve would answer today's
	// question about yesterday's plan (the branch may have been geocoded since,
	// DEPOT_LAT may have changed), and a fabricated or cached value would be
	// worse than the empty string: it would look authoritative. An empty string
	// on GET is the correct answer.
	//
	// A migration adding depot_source/depot_note columns would buy exactly one
	// thing — the warning surviving a page reload or a later fetch. Until then
	// the slog line Service.Plan emits is the durable copy, and it carries the
	// same two values word for word.
	DepotSource string `json:"depot_source,omitempty"`
	DepotNote   string `json:"depot_note,omitempty"`

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
