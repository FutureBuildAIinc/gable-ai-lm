// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package catalog manages per-product physical dimensions (L/W/H, stacking)
// keyed by GableLBM product id. GableLBM owns product weight; AI_LM stores the
// dimensional data the ERP lacks, with an optional weight override.
package catalog

import "time"

// Dimension holds the load-planning attributes for one GableLBM product.
type Dimension struct {
	ID                string    `json:"id"`
	GableProductID    string    `json:"gable_product_id"`
	SKU               string    `json:"sku"`
	LengthIn          float64   `json:"length_in"`
	WidthIn           float64   `json:"width_in"`
	HeightIn          float64   `json:"height_in"`
	Stackable         bool      `json:"stackable"`
	WeightLbsOverride *float64  `json:"weight_lbs_override,omitempty"`
	DefaultSource     string    `json:"default_source"` // UOM/MANUAL/IMPORT
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// DimensionInput is the upsert payload for product dimensions.
type DimensionInput struct {
	SKU               string   `json:"sku"`
	LengthIn          float64  `json:"length_in"`
	WidthIn           float64  `json:"width_in"`
	HeightIn          float64  `json:"height_in"`
	Stackable         bool     `json:"stackable"`
	WeightLbsOverride *float64 `json:"weight_lbs_override,omitempty"`
	DefaultSource     string   `json:"default_source"`
}

// Geometry provenance for an EffectiveProduct — where the resolved L/W/H came
// from after merging the AI_LM override row with the PIM-canonical dimensions.
const (
	GeometryOverride = "OVERRIDE" // AI_LM product_dimensions row (human/manual override)
	GeometryPIM      = "PIM"      // GableLBM PIM canonical geometry
	GeometryFallback = "FALLBACK" // neither present — no usable geometry
)

// EffectiveProduct is the resolved load-planning view of a product: GableLBM's
// catalog data merged with AI_LM's local overrides. GeometrySource records the
// winning provider and HasGeometry is false when no usable L/W/H exists so the
// Load Builder can flag the item rather than render a zero-size box.
type EffectiveProduct struct {
	GableProductID string  `json:"gable_product_id"`
	SKU            string  `json:"sku"`
	Name           string  `json:"name"`
	Category       string  `json:"category,omitempty"`
	LengthIn       float64 `json:"length_in"`
	WidthIn        float64 `json:"width_in"`
	HeightIn       float64 `json:"height_in"`
	Stackable      bool    `json:"stackable"`
	WeightLbs      float64 `json:"weight_lbs"`
	GeometrySource string  `json:"geometry_source"` // OVERRIDE / PIM / FALLBACK
	HasGeometry    bool    `json:"has_geometry"`
}
