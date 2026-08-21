// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package load builds a 3D load plan for a truck: it places order line items
// on the bed, distributes their weight across axles, and flags GVW compliance.
// The placement algorithm sits behind a Solver interface so a smarter optimizer
// (or ML model) can replace the deterministic MVP heuristic later.
//
// # Weight model: what is certified and what is advisory
//
// This package is used on a safety- and compliance-adjacent surface, so the
// accuracy of each number it emits is stated explicitly here. Callers must not
// present advisory numbers as certified.
//
// TRUSTWORTHY — Plan.TotalWeightLbs (gross combination weight) is exact: it is
// the sum of every placed unit's weight, plus any cargo that rides without a
// placement (Plan.UnmodeledWeightLbs — a dimensionless SKU has nothing to
// position but still goes on the deck), plus the profile's tare weight.
// Plan.GVWStatus compares that total against the profile's GVWR. Nothing about
// longitudinal geometry affects it.
//
// ADVISORY — every AxleLoad in Plan.AxleLoads. Each carries Advisory=true. The
// per-axle split is a planning aid, NOT a substitute for a certified scale
// ticket, because the model deliberately does not account for:
//
//   - Datum. Axle.PositionFromFrontIn is compared directly against Placement.X,
//     which is measured from the front of the BED. A real steer axle sits under
//     the cab, well ahead of the bed front, and the fleet profile carries no
//     steer setback or true wheelbase. A profile that reports the steer axle at
//     position 0 therefore compresses the wheelbase and over-attributes cargo to
//     the steer axle.
//   - Overhang. Cargo ahead of the first axle or behind the last is assigned
//     100% to that end axle (see distributeToAxles). Real overhang acts as a
//     lever: it loads the near axle by MORE than the cargo weight and unloads
//     the far axle by the balance (a steer reaction can even go negative). Those
//     signed reactions are not computed.
//   - Tare distribution. Chassis tare is apportioned in proportion to each
//     axle's rating, not from the chassis centre of gravity, because the profile
//     carries no CG.
//   - Statics only. No dynamic load transfer, no suspension behaviour, and no
//     tandem-spread / federal bridge-formula legal limits — only each axle's own
//     rating from the fleet profile.
//   - Unmodeled cargo. Plan.UnmodeledWeightLbs rides but has no placement, so it
//     is in the gross and absent from every axle.
//
// HOW CALLERS MUST GATE ON THIS. A dispatch gate may HARD-REFUSE on the exact
// numbers and must NOT hard-refuse on the advisory ones:
//
//   - Plan.GVWStatus (gross vs GVWR, and the profile-completeness roll-up) is a
//     measurement of an exact quantity — block on it.
//   - An AxleLoad with Advisory=true over its rating is this model's opinion,
//     computed on a datum it documents as wrong (the steer axle at the bed
//     origin) and without the overhang lever that would move the answer in
//     either direction. Surface it loudly — it is the reason to stop at a scale
//     — but do not refuse on it, because refusing turns an unvalidated estimate
//     into a hard cap: on the default flatbed profile it caps payload at roughly
//     half the truck's rating, which trains dispatchers to distrust the module
//     rather than to weigh the truck. ROADMAP §3 is explicit that per-axle
//     numbers stay advisory "until validated against certified scale tickets".
//   - Advisory=false is the seam for that future: a per-axle verdict sourced
//     from a calibrated/measured profile is a measurement, and a gate may block
//     on it. The zero value is therefore the blocking one — fail-closed.
//   - An axle that could not be judged at all (StatusUnknown, or any status a
//     future solver adds) is a PROFILE defect, not an advisory estimate, and
//     stays blocking. It also forces Plan.GVWStatus to FAIL — see below.
//
// UNKNOWN — when the fleet profile is missing a rating (a zero axle rating, a
// zero GVWR, or no axles at all) the affected axle status is StatusUnknown and
// Plan.ProfileStatus is ProfileIncomplete, with the specific defects listed in
// Plan.ProfileIssues. Such a plan is never reported as PASS: an unrated profile
// cannot certify anything. See Plan.GVWStatus for how UNKNOWN is surfaced on the
// overall verdict.
package load

import "time"

// Item is one product line to be loaded (already resolved to dimensions/weight).
type Item struct {
	ProductID string  `json:"product_id"`
	SKU       string  `json:"sku"`
	Quantity  int     `json:"quantity"`
	LengthIn  float64 `json:"length_in"`
	WidthIn   float64 `json:"width_in"`
	HeightIn  float64 `json:"height_in"`
	WeightLbs float64 `json:"weight_lbs"` // per-unit weight
	// Stackable reports whether this article may be stacked — in BOTH
	// directions. A non-stackable article (natural-stone slab, crated window,
	// 6x6 PT post, palletized or bagged goods) is never banded more than one
	// unit high and never has anything placed on top of it, so the pack manifest
	// and the 3D twin never instruct yard staff to build a load that crushes it.
	Stackable bool `json:"stackable"`
}

// Vehicle is the subset of a fleet profile the solver needs.
type Vehicle struct {
	GableVehicleID string
	BedLengthIn    float64
	BedWidthIn     float64
	BedHeightIn    float64
	GVWRLbs        int64
	TareWeightLbs  int64
	Axles          []Axle

	// Securement inputs (T1-5/T2-7). SecurementJurisdiction selects the
	// load-securement ruleset; AnchorSpacingIn models the winch-track / D-ring
	// tie-down anchor pitch along the bed (0 ⇒ a sensible default) so straps are
	// optimized onto real anchor points.
	SecurementJurisdiction string
	AnchorSpacingIn        float64
}

// Axle is a rated axle at a longitudinal position.
type Axle struct {
	AxleNumber          int
	MaxWeightLbs        int64
	PositionFromFrontIn float64
	AxleType            string
}

// Placement is one positioned unit box in the 3D scene. Coordinates are inches
// from the front-left-floor corner of the bed: X = length (front→back),
// Y = width (left→right), Z = height (floor→up).
//
// For sequenced (multi-stop) plans, OrderID/StopSequence tie the unit to its
// delivery stop and Step is its 1-based position in the physical packing order
// (step 1 is loaded first, at the nose; the first stop's material is loaded
// last so it comes off first).
type Placement struct {
	ItemID    string  `json:"item_id"`
	SKU       string  `json:"sku"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Z         float64 `json:"z"`
	LengthIn  float64 `json:"length_in"`
	WidthIn   float64 `json:"width_in"`
	HeightIn  float64 `json:"height_in"`
	WeightLbs float64 `json:"weight_lbs"`
	AxleGroup int     `json:"axle_group"` // nearest axle number, for color coding

	OrderID      string `json:"order_id,omitempty"`
	StopSequence int    `json:"stop_sequence,omitempty"`
	Step         int    `json:"step,omitempty"`
}

// Load-plan verdicts. StatusUnknown is NOT a degraded PASS: it means the fleet
// profile did not supply the rating needed to judge, so no verdict is possible.
const (
	StatusPass    = "PASS"
	StatusWarn    = "WARN"
	StatusFail    = "FAIL"
	StatusUnknown = "UNKNOWN"
)

// Fleet-profile completeness verdicts for Plan.ProfileStatus.
const (
	ProfileComplete   = "COMPLETE"
	ProfileIncomplete = "INCOMPLETE"
)

// AxleLoad is the computed load on one axle vs its rating.
//
// Advisory is true for everything this solver produces: the per-axle split is a
// planning aid, not a certified scale ticket. See the package doc for exactly
// what the model does and does not account for (bed-origin datum, no overhang
// reactions, tare split by axle rating rather than chassis CG). Callers MUST NOT
// present these numbers as certified, and a dispatch gate must warn rather than
// refuse on an Advisory over-rating — see "HOW CALLERS MUST GATE ON THIS" in the
// package doc.
type AxleLoad struct {
	AxleNumber   int     `json:"axle_number"`
	WeightLbs    int64   `json:"weight_lbs"`
	MaxWeightLbs int64   `json:"max_weight_lbs"`
	Utilization  float64 `json:"utilization"` // weight / max; 0 when unrated
	Status       string  `json:"status"`      // PASS/WARN/FAIL/UNKNOWN
	Advisory     bool    `json:"advisory"`    // always true — verify at a certified scale
}

// Plan is the full solver output, persisted and returned to the 3D view.
type Plan struct {
	ID              string      `json:"id"`
	GableRouteID    *string     `json:"gable_route_id,omitempty"`
	GableDeliveryID *string     `json:"gable_delivery_id,omitempty"`
	GableVehicleID  string      `json:"gable_vehicle_id"`
	Placements      []Placement `json:"placements"`
	TotalWeightLbs  int64       `json:"total_weight_lbs"`
	AxleLoads       []AxleLoad  `json:"axle_loads"`
	BalanceScore    float64     `json:"balance_score"` // 0..1, higher = better

	// GVWStatus is the overall weight verdict: PASS/WARN/FAIL. It is the worst
	// of every rated axle's status and the gross-vs-GVWR status.
	//
	// An UNKNOWN input (a missing axle rating or GVWR — see ProfileStatus) is
	// blocking and reports here as FAIL, never PASS: with no rating there is
	// nothing to be compliant against. FAIL rather than a literal "UNKNOWN" is
	// emitted because this field is a three-value enum in the published API
	// contract and its consumers key maps by it; the precise reason is on
	// ProfileStatus/ProfileIssues, which callers should render as
	// "profile incomplete — results not trustworthy".
	GVWStatus string `json:"gvw_status"` // PASS/WARN/FAIL

	// Unplaced is every article the packer did not position, each with a TYPED
	// reason. The two families are not interchangeable: a capacity reason means
	// the cargo stays in the yard and the customer ships short, whereas
	// ReasonNoGeometry means the article has no recorded dimensions to place —
	// it still rides, it is simply absent from the twin. Callers gating on
	// "can this truck go?" must branch on Unplaced.Blocking(), never on len().
	Unplaced []Unplaced `json:"unplaced"`

	// UnmodeledWeightLbs is the weight of cargo that rides but has no placement:
	// the Unplaced entries whose Rides() is true. It is already INCLUDED in
	// TotalWeightLbs (gross stays exact even when the twin is incomplete) and is
	// necessarily ABSENT from AxleLoads, because there is no position to
	// attribute it to. Non-zero ⇒ the per-axle split understates the truck.
	UnmodeledWeightLbs int64 `json:"unmodeled_weight_lbs,omitempty"`

	MaxLoadHeightIn float64 `json:"max_load_height_in,omitempty"`

	// ProfileStatus reports whether the fleet profile carried everything needed
	// to judge this load: ProfileComplete or ProfileIncomplete. ProfileIssues
	// lists each specific defect (blank axle rating, blank GVWR, no axles). When
	// ProfileStatus is ProfileIncomplete the weight verdicts on this plan are not
	// trustworthy and the UI must say so rather than showing a green badge.
	ProfileStatus string   `json:"profile_status,omitempty"`
	ProfileIssues []string `json:"profile_issues,omitempty"`

	// Volume budget (T2-2): a high-volume / low-weight load can max out the bed
	// before it maxes out the axles. CargoVolumeCuFt is the placed bounding
	// volume; UsableVolumeCuFt is the bed envelope discounted for real packing
	// efficiency; VolumeStatus mirrors the axle PASS/WARN/FAIL scale.
	BedVolumeCuFt     float64 `json:"bed_volume_cuft,omitempty"`
	UsableVolumeCuFt  float64 `json:"usable_volume_cuft,omitempty"`
	CargoVolumeCuFt   float64 `json:"cargo_volume_cuft,omitempty"`
	VolumeUtilization float64 `json:"volume_utilization,omitempty"`
	VolumeStatus      string  `json:"volume_status,omitempty"` // PASS/WARN/FAIL

	Securement *Securement `json:"securement,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
}

// StopItems is one delivery stop's resolved line items for sequenced packing.
type StopItems struct {
	OrderID      string `json:"order_id"`
	StopSequence int    `json:"stop_sequence"`
	Items        []Item `json:"items"`
}

// Strap is one tie-down across the load at a longitudinal position. PositionIn
// is snapped to the nearest modeled bed anchor so the recommendation lands on a
// real tie-down point.
type Strap struct {
	Number         int     `json:"number"`
	PositionIn     float64 `json:"position_in"`      // inches from the bed front (anchored)
	OverHeightIn   float64 `json:"over_height_in"`   // load height under the strap
	RequiredWLLLbs int64   `json:"required_wll_lbs"` // working-load-limit share
}

// Securement is the tie-down plan for a packed load, derived from a configurable
// jurisdiction load-securement ruleset (US FMCSA / Canada NSC Standard 10 …):
// aggregate working load limit ≥ a fraction of cargo weight, plus a minimum
// tie-down count by article length / weight / max-spacing. Strap positions are
// optimized onto the modeled bed anchor points. The rule basis is surfaced so
// the recommendation is auditable.
type Securement struct {
	CargoWeightLbs     int64   `json:"cargo_weight_lbs"`
	MinAggregateWLLLbs int64   `json:"min_aggregate_wll_lbs"`
	Straps             []Strap `json:"straps"`
	RecommendedStrap   string  `json:"recommended_strap"`

	// Rule basis (T2-7) — which jurisdiction ruleset produced this plan.
	Jurisdiction     string  `json:"jurisdiction"`
	RulesetName      string  `json:"ruleset_name"`
	RuleBasis        string  `json:"rule_basis"`
	RequiredTieDowns int     `json:"required_tie_downs"` // ruleset minimum (Straps may add for WLL)
	AnchorSpacingIn  float64 `json:"anchor_spacing_in,omitempty"`

	Notes []string `json:"notes"`
}
