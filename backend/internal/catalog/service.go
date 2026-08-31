// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package catalog

import (
	"context"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/metrics"
)

// productSource fetches the PIM-canonical catalog (with geometry + weight) from
// GableLBM. Satisfied by *gable.Client; nil-able so the service degrades to an
// overrides-only view when no integration client is wired.
type productSource interface {
	GetProductsWithWeight(ctx context.Context) ([]gable.Product, error)
}

// dimensionStore is the persistence seam for AI_LM's dimension overrides
// (satisfied by *Repository). It is declared consumer-side, the same way
// internal/workflow declares planStore, so this module's own logic — which
// geometry wins, and which round-trip counts as a catalog pull — can be
// exercised without a Postgres.
type dimensionStore interface {
	List(ctx context.Context) ([]Dimension, error)
	GetByProductID(ctx context.Context, gableProductID string) (*Dimension, error)
	Upsert(ctx context.Context, gableProductID string, in DimensionInput) (*Dimension, error)
}

// Service holds product-dimension business logic.
type Service struct {
	repo     dimensionStore
	products productSource
	meter    *metrics.Meter
}

// NewService wires the dimension repository and the GableLBM product source.
// products may be nil (overrides-only mode for tests / offline use).
func NewService(repo dimensionStore, products productSource) *Service {
	return &Service{repo: repo, products: products}
}

// WithMeter attaches the business-metering counters (pkg/metrics) so a catalog
// pull lands on /metrics under this deployment's licence labels. cmd/server
// calls it once at boot; the meter is nil-safe, so a Service built without one
// meters nothing and behaves exactly as it did before.
func (s *Service) WithMeter(m *metrics.Meter) *Service {
	s.meter = m
	return s
}

// ListDimensions returns all product dimension records.
func (s *Service) ListDimensions(ctx context.Context) ([]Dimension, error) {
	return s.repo.List(ctx)
}

// GetDimension returns dimensions for one product, or ErrNotFound.
func (s *Service) GetDimension(ctx context.Context, gableProductID string) (*Dimension, error) {
	return s.repo.GetByProductID(ctx, gableProductID)
}

// UpsertDimension creates or replaces a product's dimensions.
func (s *Service) UpsertDimension(ctx context.Context, gableProductID string, in DimensionInput) (*Dimension, error) {
	return s.repo.Upsert(ctx, gableProductID, in)
}

// ListEffectiveProducts returns the resolved load-planning catalog: every
// GableLBM product merged with its AI_LM override (if any). Priority per
// product is AI_LM override → PIM canonical geometry → fallback. When no
// product source is wired it degrades to the override rows alone.
func (s *Service) ListEffectiveProducts(ctx context.Context) ([]EffectiveProduct, error) {
	overrides, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list overrides: %w", err)
	}
	byID := make(map[string]Dimension, len(overrides))
	for _, d := range overrides {
		byID[d.GableProductID] = d
	}

	// Overrides-only mode: no integration client wired.
	if s.products == nil {
		out := make([]EffectiveProduct, 0, len(overrides))
		for _, d := range overrides {
			d := d
			out = append(out, resolveGeometry(gable.Product{
				ID:        d.GableProductID,
				SKU:       d.SKU,
				WeightLbs: 0,
			}, &d))
		}
		return out, nil
	}

	products, err := s.products.GetProductsWithWeight(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch PIM products: %w", err)
	}
	// Metered here and only here: a pull is an answered round-trip to the ERP's
	// PIM. The overrides-only path above never reached it, and a failed fetch
	// returned no catalog — neither is a pull.
	s.meter.CatalogPulled()

	out := make([]EffectiveProduct, 0, len(products))
	for _, p := range products {
		var ovr *Dimension
		if d, ok := byID[p.ID]; ok {
			d := d
			ovr = &d
		}
		out = append(out, resolveGeometry(p, ovr))
	}
	return out, nil
}

// resolveGeometry merges a GableLBM product with an optional AI_LM override row
// into the effective load-planning view. Geometry priority:
//  1. OVERRIDE — the override row carries non-zero L/W/H (a human-entered value).
//  2. PIM      — the GableLBM product carries non-nil, non-zero L/W/H.
//  3. FALLBACK — neither present; HasGeometry is false.
//
// Weight prefers the override's WeightLbsOverride, else the PIM weight.
// Stackable prefers the override row, else the PIM flag (default true).
func resolveGeometry(p gable.Product, ovr *Dimension) EffectiveProduct {
	ep := EffectiveProduct{
		GableProductID: p.ID,
		SKU:            p.SKU,
		Name:           p.Name,
		Category:       p.Category,
		WeightLbs:      p.WeightLbs,
		Stackable:      true,
	}

	// PIM-provided stackable (default true when unset).
	if p.Stackable != nil {
		ep.Stackable = *p.Stackable
	}

	overrideHasDims := ovr != nil && ovr.LengthIn > 0 && ovr.WidthIn > 0 && ovr.HeightIn > 0
	pimHasDims := p.LengthIn != nil && p.WidthIn != nil && p.HeightIn != nil &&
		*p.LengthIn > 0 && *p.WidthIn > 0 && *p.HeightIn > 0

	switch {
	case overrideHasDims:
		ep.LengthIn = ovr.LengthIn
		ep.WidthIn = ovr.WidthIn
		ep.HeightIn = ovr.HeightIn
		ep.Stackable = ovr.Stackable
		ep.GeometrySource = GeometryOverride
		ep.HasGeometry = true
	case pimHasDims:
		ep.LengthIn = *p.LengthIn
		ep.WidthIn = *p.WidthIn
		ep.HeightIn = *p.HeightIn
		ep.GeometrySource = GeometryPIM
		ep.HasGeometry = true
	default:
		ep.GeometrySource = GeometryFallback
		ep.HasGeometry = false
	}

	// Weight override wins regardless of which geometry source did.
	if ovr != nil && ovr.WeightLbsOverride != nil {
		ep.WeightLbs = *ovr.WeightLbsOverride
	}

	return ep
}
