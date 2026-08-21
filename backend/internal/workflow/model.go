// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package workflow orchestrates the guided end-to-end dispatch flow:
//
//  1. Ingest    — pull a calendar date's confirmed GableLBM orders and deep-
//     analyze each one (per-line geometry, weight, volume, shape).
//  2. Assign    — split orders across trucks (CVRP) and sequence each route.
//  3. Pack      — 3D-pack every truck LIFO by stop (last stop loads first) as
//     realistic banded lumber bundles; routes may be resequenced.
//  4. Review    — check every route against restricted points (bridge weight
//     limits, overpass clearances) and auto-resolve flags by
//     rerouting or re-balancing loads across trucks.
//  5. Push      — write the final routes + packing manifests to the GableLBM
//     dispatch board (and its yard Pack Trucks surface).
//
// One Plan row carries the whole run; each step persists its artifacts so the
// UI can resume/replay any stage.
package workflow

import (
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/load"
)

// Plan statuses, in workflow order.
const (
	StatusAnalyzed = "ANALYZED"
	StatusAssigned = "ASSIGNED"
	StatusPacked   = "PACKED"
	StatusReviewed = "REVIEWED"
	StatusPushed   = "PUSHED"
)

// Shape profiles summarize what kind of load an order is (drives truck choice).
const (
	ShapeLongLoad = "LONG_LOAD" // longest piece ≥ 16 ft
	ShapeCompact  = "COMPACT"   // everything under 8 ft
	ShapeMixed    = "MIXED"
)

// AnalyzedLine is one order line resolved against the effective catalog
// (override → PIM → fallback geometry) with derived per-line totals.
type AnalyzedLine struct {
	ProductID      string  `json:"product_id"`
	SKU            string  `json:"sku"`
	Name           string  `json:"name,omitempty"`
	Quantity       float64 `json:"quantity"`
	UnitWeightLbs  float64 `json:"unit_weight_lbs"`
	UnitLengthIn   float64 `json:"unit_length_in"`
	UnitWidthIn    float64 `json:"unit_width_in"`
	UnitHeightIn   float64 `json:"unit_height_in"`
	Stackable      bool    `json:"stackable"`
	HasGeometry    bool    `json:"has_geometry"`
	LineWeightLbs  float64 `json:"line_weight_lbs"`
	LineVolumeCuFt float64 `json:"line_volume_cuft"`
	// DimOverride records a per-order dimension override for a variable-
	// dimension SKU (one whose size varies by order, e.g. natural-stone steps).
	// When present the unit L/W/H above are its resolved (upper-bound) dims.
	DimOverride *DimOverride `json:"dim_override,omitempty"`
}

// DimOverride is a per-order/per-line dimension override (T2-2). For a
// variable-dimension SKU the dispatcher supplies this order's actual size; when
// only an average is known a tolerance grows it to a planning upper bound so the
// digital twin + packing reserve room for the largest likely piece.
type DimOverride struct {
	LengthIn     float64 `json:"length_in"`
	WidthIn      float64 `json:"width_in"`
	HeightIn     float64 `json:"height_in"`
	TolerancePct float64 `json:"tolerance_pct,omitempty"` // applied to reach the upper bound
	Source       string  `json:"source,omitempty"`        // MEASURED / AVERAGE
	Note         string  `json:"note,omitempty"`
}

// OrderAnalysis is the deep analysis of one ingested order.
type OrderAnalysis struct {
	OrderID         string         `json:"order_id"`
	CustomerName    string         `json:"customer_name,omitempty"`
	Address         string         `json:"address,omitempty"`
	Lat             *float64       `json:"lat,omitempty"`
	Lng             *float64       `json:"lng,omitempty"`
	Lines           []AnalyzedLine `json:"lines"`
	TotalWeightLbs  float64        `json:"total_weight_lbs"`
	TotalVolumeCuFt float64        `json:"total_volume_cuft"`
	MaxLengthIn     float64        `json:"max_length_in"`
	PieceCount      int            `json:"piece_count"`
	ShapeProfile    string         `json:"shape_profile"`
	Routable        bool           `json:"routable"`
	// Priority marks this order for preferred (deliver-first) handling: the
	// assign/sequence step pins priority stops to the front of their truck's
	// route, then optimizes the rest around them (dealer override T2-1).
	Priority bool     `json:"priority"`
	Issues   []string `json:"issues"`
}

// Stop is one sequenced delivery stop on a truck. Priority mirrors the order's
// deliver-first flag so the UI can render it without a separate lookup.
type Stop struct {
	OrderID      string  `json:"order_id"`
	Sequence     int     `json:"sequence"`
	Lat          float64 `json:"lat"`
	Lng          float64 `json:"lng"`
	Address      string  `json:"address,omitempty"`
	CustomerName string  `json:"customer_name,omitempty"`
	WeightLbs    float64 `json:"weight_lbs"`
	Priority     bool    `json:"priority"`
}

// BedDims is the truck bed envelope from the fleet profile (for the 3D view).
type BedDims struct {
	LengthIn float64 `json:"length_in"`
	WidthIn  float64 `json:"width_in"`
	HeightIn float64 `json:"height_in"`
}

// ComplianceAction is one automatic resolution the reviewer applied (or tried).
type ComplianceAction struct {
	Type        string `json:"type"` // REROUTE / LOAD_ADJUST / MANUAL_REVIEW
	Description string `json:"description"`
	Resolved    bool   `json:"resolved"`
}

// ComplianceReview is the restricted-point check outcome for one truck route.
// Detours are AI-inserted waypoints that steer the route polyline around
// flagged restrictions (rendered on the route map; not pushed to GableLBM).
type ComplianceReview struct {
	Status  string             `json:"status"` // PASS/WARN/FAIL
	Flags   []compliance.Flag  `json:"flags"`
	Actions []ComplianceAction `json:"actions"`
	// Advisories are capacity findings the dispatcher must SEE but may proceed
	// past: an over-rating on the per-axle estimate (whose datum the load package
	// documents as unvalidated), and lines with no digital-twin geometry (which
	// ride, but are not in the 3D plan). They never refuse a push — the UI is
	// required to render them, because an advisory nobody sees is worse than no
	// advisory at all. See workflow.capacityFindings.
	Advisories        []string                `json:"advisories,omitempty"`
	Detours           []compliance.RoutePoint `json:"detours,omitempty"`
	CheckedGrossLbs   int64                   `json:"checked_gross_lbs"`
	CheckedMaxAxleLbs int64                   `json:"checked_max_axle_lbs"`
	CheckedHeightIn   float64                 `json:"checked_height_in"`
}

// TruckLoad is one truck's full workflow artifact: route, packing, compliance.
type TruckLoad struct {
	VehicleID         string            `json:"vehicle_id"`
	VehicleName       string            `json:"vehicle_name"`
	DriverID          string            `json:"driver_id,omitempty"`
	DriverName        string            `json:"driver_name,omitempty"`
	CapacityWeightLbs int               `json:"capacity_weight_lbs"`
	Stops             []Stop            `json:"stops"`
	TotalWeightLbs    float64           `json:"total_weight_lbs"`
	TotalDistanceMi   float64           `json:"total_distance_mi"`
	TotalDurationMin  float64           `json:"total_duration_min"`
	Bed               *BedDims          `json:"bed,omitempty"`
	LoadPlan          *load.Plan        `json:"load_plan,omitempty"`
	Compliance        *ComplianceReview `json:"compliance,omitempty"`
	// Proof is the yard proof-of-load + sign-off (T1-6). A truck cannot be
	// pushed (leave the yard) until Proof has at least one attachment and a
	// sign-off.
	Proof *LoadProof `json:"proof,omitempty"`
}

// ProofAttachment is one yard photo/video reference for a packed load (T1-6).
// A demo-appropriate store: a URL + metadata, persisted in the plan JSONB.
type ProofAttachment struct {
	URL     string    `json:"url"`
	Kind    string    `json:"kind"` // PHOTO / VIDEO
	Caption string    `json:"caption,omitempty"`
	AddedBy string    `json:"added_by,omitempty"`
	AddedAt time.Time `json:"added_at"`
}

// LoadProof is the yard proof-of-load + sign-off for one truck (T1-6).
type LoadProof struct {
	Attachments []ProofAttachment `json:"attachments"`
	SignedOff   bool              `json:"signed_off"`
	SignedBy    string            `json:"signed_by,omitempty"`
	SignedRole  string            `json:"signed_role,omitempty"`
	SignedAt    *time.Time        `json:"signed_at,omitempty"`
	Note        string            `json:"note,omitempty"`
}

// Ready reports whether the load satisfies the depart gate: at least one proof
// attachment and a sign-off.
func (p *LoadProof) Ready() bool {
	return p != nil && len(p.Attachments) > 0 && p.SignedOff
}

// Lock window codes (T2-3).
const (
	LockWindowMorning   = "MORNING"
	LockWindowAfternoon = "AFTERNOON"
	LockWindowCustom    = "CUSTOM"
)

// Late-add statuses (T2-3).
const (
	LateAddPending  = "PENDING"
	LateAddApproved = "APPROVED"
	LateAddRejected = "REJECTED"
)

// PlanLock models a run's scheduled-lock state (T2-3). A locked run is not
// silently re-shuffled: assign / resequence / priority changes require an
// explicit override (manual approval), and a late same-day order add is queued
// for approval instead of reshuffling. Locked is the effective state; a Window +
// LockAt schedule auto-locks the run once that time passes on the plan date.
type PlanLock struct {
	Locked   bool       `json:"locked"`
	Window   string     `json:"window,omitempty"`  // MORNING / AFTERNOON / CUSTOM
	LockAt   string     `json:"lock_at,omitempty"` // HH:MM the window auto-locks
	LockedBy string     `json:"locked_by,omitempty"`
	LockedAt *time.Time `json:"locked_at,omitempty"`
	Reason   string     `json:"reason,omitempty"`
}

// LateAdd is a same-day order added to a locked run, awaiting approval (T2-3).
type LateAdd struct {
	OrderID      string    `json:"order_id"`
	CustomerName string    `json:"customer_name,omitempty"`
	Status       string    `json:"status"` // PENDING / APPROVED / REJECTED
	RequestedBy  string    `json:"requested_by,omitempty"`
	RequestedAt  time.Time `json:"requested_at"`
	ResolvedBy   string    `json:"resolved_by,omitempty"`
	Note         string    `json:"note,omitempty"`
}

// Plan is one end-to-end workflow run for a delivery date.
type Plan struct {
	ID       string `json:"id"`
	PlanDate string `json:"plan_date"` // YYYY-MM-DD
	Status   string `json:"status"`
	// Version is the optimistic-concurrency token. Every mutation reads a plan,
	// changes it, and writes it back with `WHERE version = <the value it read>`;
	// a concurrent writer that already bumped the row makes the second write
	// affect zero rows and fail with ErrVersionConflict (HTTP 409) instead of
	// silently discarding the other actor's change. Clients round-trip it so a
	// stale tab can be told to reload rather than clobber.
	Version  int     `json:"version"`
	DepotLat float64 `json:"depot_lat"`
	DepotLng float64 `json:"depot_lng"`
	// DepotSource records where the routing origin came from: REQUEST / CONFIG /
	// CENTROID / NONE.
	DepotSource      string          `json:"depot_source,omitempty"`
	Orders           []OrderAnalysis `json:"orders"`
	Loads            []TruckLoad     `json:"loads"`
	UnassignedOrders []Stop          `json:"unassigned_orders"`
	Lock             *PlanLock       `json:"lock,omitempty"`
	LateAdds         []LateAdd       `json:"late_adds,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// IngestRequest starts a workflow run for a date.
type IngestRequest struct {
	Date     string   `json:"date"` // YYYY-MM-DD
	DepotLat *float64 `json:"depot_lat,omitempty"`
	DepotLng *float64 `json:"depot_lng,omitempty"`
}

// ResequenceRequest manually reorders one truck's stops (triggers a re-pack).
// Override authorizes the change on a locked run (T2-3).
type ResequenceRequest struct {
	OrderIDs   []string `json:"order_ids"`
	Override   bool     `json:"override,omitempty"`
	ApprovedBy string   `json:"approved_by,omitempty"`
}

// PriorityRequest toggles an order's deliver-first flag (dealer override T2-1),
// re-sequencing (and re-packing) the affected truck. Override authorizes the
// change on a locked run (T2-3).
type PriorityRequest struct {
	Priority   bool   `json:"priority"`
	Override   bool   `json:"override,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
}

// AssignRequest runs (or re-runs) truck assignment. Override authorizes a
// re-assignment on a locked run (T2-3); the body may be empty.
type AssignRequest struct {
	Override   bool   `json:"override,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
}

// ProofRequest attaches one yard photo/video reference to a packed load (T1-6).
type ProofRequest struct {
	URL     string `json:"url"`
	Kind    string `json:"kind,omitempty"` // PHOTO / VIDEO (default PHOTO)
	Caption string `json:"caption,omitempty"`
	AddedBy string `json:"added_by,omitempty"`
}

// SignOffRequest records the yard sign-off that releases a load to depart (T1-6).
type SignOffRequest struct {
	SignedBy string `json:"signed_by"`
	Role     string `json:"role,omitempty"`
	Note     string `json:"note,omitempty"`
}

// LockRequest sets a run's lock / scheduled-lock state (T2-3). Locked is a
// pointer so an omitted field defaults to an immediate lock; pass false with a
// Window to schedule a future auto-lock without locking now.
type LockRequest struct {
	Locked   *bool  `json:"locked,omitempty"`
	Window   string `json:"window,omitempty"`  // MORNING / AFTERNOON / CUSTOM
	LockAt   string `json:"lock_at,omitempty"` // HH:MM (required for CUSTOM)
	Reason   string `json:"reason,omitempty"`
	LockedBy string `json:"locked_by,omitempty"`
}

// LateAddRequest queues a late same-day order onto a run (T2-3). When the run is
// locked it is recorded PENDING (needs approval) instead of reshuffling.
type LateAddRequest struct {
	OrderID     string `json:"order_id"`
	RequestedBy string `json:"requested_by,omitempty"`
	Note        string `json:"note,omitempty"`
}

// LateAddApproveRequest approves (or rejects) a queued late add (T2-3).
type LateAddApproveRequest struct {
	Reject     bool   `json:"reject,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
}

// DimensionOverrideRequest sets this order's dimensions for a variable-dimension
// SKU (T2-2). The line is matched by ProductID when given, else by SKU. L/W/H
// must be positive; re-ingesting the date restores the catalog geometry.
type DimensionOverrideRequest struct {
	ProductID    string  `json:"product_id,omitempty"`
	SKU          string  `json:"sku,omitempty"`
	LengthIn     float64 `json:"length_in"`
	WidthIn      float64 `json:"width_in"`
	HeightIn     float64 `json:"height_in"`
	TolerancePct float64 `json:"tolerance_pct,omitempty"`
	Source       string  `json:"source,omitempty"` // MEASURED / AVERAGE
	Note         string  `json:"note,omitempty"`
}

// Briefing is the LLM-generated dispatch briefing for a plan. When AI is not
// configured Available is false and Message explains how to enable it; the core
// workflow is never blocked on it.
type Briefing struct {
	Available bool   `json:"available"`
	Model     string `json:"model,omitempty"`
	Text      string `json:"text,omitempty"`
	Message   string `json:"message,omitempty"`
}
