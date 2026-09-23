// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/metrics"
	dto "github.com/prometheus/client_model/go"
)

// ailm_catalog_pulls_total counts ANSWERED round-trips to the ERP's PIM.
//
// The two paths that look like pulls and are not: the overrides-only mode
// (there is no product source wired, so nothing was fetched) and a failed fetch
// (the ERP did not answer, so there is no catalog). Counting either would
// inflate a number that is meant to describe integration traffic.

type fakeDimensions struct {
	dims []Dimension
	err  error
}

func (f *fakeDimensions) List(context.Context) ([]Dimension, error) { return f.dims, f.err }
func (f *fakeDimensions) GetByProductID(context.Context, string) (*Dimension, error) {
	return nil, ErrNotFound
}
func (f *fakeDimensions) Upsert(context.Context, string, DimensionInput) (*Dimension, error) {
	return nil, errors.New("not used")
}

type fakeProducts struct {
	products []gable.Product
	err      error
	calls    int
}

func (f *fakeProducts) GetProductsWithWeight(context.Context) ([]gable.Product, error) {
	f.calls++
	return f.products, f.err
}

func catalogPulls(t *testing.T, subject string) float64 {
	t.Helper()
	c, err := metrics.CatalogPullsTotal.GetMetricWithLabelValues("community", subject)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestCatalogPullsAreMeteredOnlyWhenTheERPAnswers(t *testing.T) {
	tests := []struct {
		name     string
		subject  string
		products *fakeProducts // nil ⇒ overrides-only mode
		wantErr  bool
		want     float64
	}{
		{
			name:     "an answered pull counts once",
			subject:  "meter-catalog-ok",
			products: &fakeProducts{products: []gable.Product{{ID: "p1", SKU: "2x4-8", WeightLbs: 40}}},
			want:     1,
		},
		{
			name:     "an ERP that did not answer is not a pull",
			subject:  "meter-catalog-erp-down",
			products: &fakeProducts{err: errors.New("GableLBM 502")},
			wantErr:  true,
			want:     0,
		},
		{
			name:    "overrides-only mode never reaches the ERP",
			subject: "meter-catalog-offline",
			want:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			meter := metrics.NewMeter("community", tc.subject)
			var src productSource
			if tc.products != nil {
				src = tc.products
			}
			svc := NewService(&fakeDimensions{}, src).WithMeter(meter)

			_, err := svc.ListEffectiveProducts(context.Background())
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("list effective products: %v", err)
			}
			if got := catalogPulls(t, tc.subject); got != tc.want {
				t.Fatalf("ailm_catalog_pulls_total = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCatalogServiceWithoutAMeterStillWorks: the meter is a pure addition
// behind a nil-safe accessor, so the module runs unchanged without one.
func TestCatalogServiceWithoutAMeterStillWorks(t *testing.T) {
	svc := NewService(&fakeDimensions{}, &fakeProducts{products: []gable.Product{{ID: "p1", SKU: "2x4-8"}}})
	got, err := svc.ListEffectiveProducts(context.Background())
	if err != nil {
		t.Fatalf("a Service with no meter must behave exactly as before: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d effective products, want 1", len(got))
	}
}
