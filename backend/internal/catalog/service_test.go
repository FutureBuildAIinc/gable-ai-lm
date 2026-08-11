// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package catalog

import (
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

func fptr(f float64) *float64 { return &f }
func bptr(b bool) *bool       { return &b }

func TestResolveGeometry(t *testing.T) {
	tests := []struct {
		name        string
		product     gable.Product
		override    *Dimension
		wantL       float64
		wantW       float64
		wantH       float64
		wantStack   bool
		wantWeight  float64
		wantSource  string
		wantHasGeom bool
	}{
		{
			name: "override wins over PIM",
			product: gable.Product{
				ID: "p1", SKU: "SKU1", WeightLbs: 10,
				LengthIn: fptr(96), WidthIn: fptr(48), HeightIn: fptr(4), Stackable: bptr(true),
			},
			override: &Dimension{LengthIn: 100, WidthIn: 50, HeightIn: 5, Stackable: false},
			wantL:    100, wantW: 50, wantH: 5,
			wantStack:   false,
			wantWeight:  10,
			wantSource:  GeometryOverride,
			wantHasGeom: true,
		},
		{
			name: "PIM used when no override dims",
			product: gable.Product{
				ID: "p2", SKU: "SKU2", WeightLbs: 7,
				LengthIn: fptr(96), WidthIn: fptr(48), HeightIn: fptr(4), Stackable: bptr(false),
			},
			override: nil,
			wantL:    96, wantW: 48, wantH: 4,
			wantStack:   false,
			wantWeight:  7,
			wantSource:  GeometryPIM,
			wantHasGeom: true,
		},
		{
			name: "zero-dim override falls through to PIM",
			product: gable.Product{
				ID: "p3", SKU: "SKU3", WeightLbs: 3,
				LengthIn: fptr(12), WidthIn: fptr(12), HeightIn: fptr(12),
			},
			override: &Dimension{LengthIn: 0, WidthIn: 0, HeightIn: 0},
			wantL:    12, wantW: 12, wantH: 12,
			wantStack:   true,
			wantWeight:  3,
			wantSource:  GeometryPIM,
			wantHasGeom: true,
		},
		{
			name:     "fallback when neither has dims",
			product:  gable.Product{ID: "p4", SKU: "SKU4", WeightLbs: 5},
			override: nil,
			wantL:    0, wantW: 0, wantH: 0,
			wantStack:   true,
			wantWeight:  5,
			wantSource:  GeometryFallback,
			wantHasGeom: false,
		},
		{
			name: "weight override applied with PIM geometry",
			product: gable.Product{
				ID: "p5", SKU: "SKU5", WeightLbs: 8,
				LengthIn: fptr(24), WidthIn: fptr(24), HeightIn: fptr(2),
			},
			override: &Dimension{WeightLbsOverride: fptr(99)},
			wantL:    24, wantW: 24, wantH: 2,
			wantStack:   true,
			wantWeight:  99,
			wantSource:  GeometryPIM,
			wantHasGeom: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveGeometry(tt.product, tt.override)
			if got.LengthIn != tt.wantL || got.WidthIn != tt.wantW || got.HeightIn != tt.wantH {
				t.Errorf("dims = (%v,%v,%v), want (%v,%v,%v)",
					got.LengthIn, got.WidthIn, got.HeightIn, tt.wantL, tt.wantW, tt.wantH)
			}
			if got.Stackable != tt.wantStack {
				t.Errorf("stackable = %v, want %v", got.Stackable, tt.wantStack)
			}
			if got.WeightLbs != tt.wantWeight {
				t.Errorf("weight = %v, want %v", got.WeightLbs, tt.wantWeight)
			}
			if got.GeometrySource != tt.wantSource {
				t.Errorf("source = %q, want %q", got.GeometrySource, tt.wantSource)
			}
			if got.HasGeometry != tt.wantHasGeom {
				t.Errorf("has_geometry = %v, want %v", got.HasGeometry, tt.wantHasGeom)
			}
		})
	}
}
