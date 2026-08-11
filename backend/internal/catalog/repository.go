// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when product dimensions do not exist.
var ErrNotFound = errors.New("product dimensions not found")

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// List returns all product dimension records.
func (r *Repository) List(ctx context.Context) ([]Dimension, error) {
	ex := r.db.GetExecutor(ctx)
	rows, err := ex.Query(ctx, `
		SELECT id, gable_product_id, sku, length_in, width_in, height_in, stackable,
		       weight_lbs_override, default_source, created_at, updated_at
		FROM product_dimensions
		ORDER BY sku`)
	if err != nil {
		return nil, fmt.Errorf("query product_dimensions: %w", err)
	}
	defer rows.Close()

	var dims []Dimension
	for rows.Next() {
		var d Dimension
		if err := rows.Scan(&d.ID, &d.GableProductID, &d.SKU, &d.LengthIn, &d.WidthIn, &d.HeightIn,
			&d.Stackable, &d.WeightLbsOverride, &d.DefaultSource, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan product_dimension: %w", err)
		}
		dims = append(dims, d)
	}
	return dims, rows.Err()
}

// GetByProductID returns dimensions for a single GableLBM product id.
func (r *Repository) GetByProductID(ctx context.Context, gableProductID string) (*Dimension, error) {
	ex := r.db.GetExecutor(ctx)
	var d Dimension
	err := ex.QueryRow(ctx, `
		SELECT id, gable_product_id, sku, length_in, width_in, height_in, stackable,
		       weight_lbs_override, default_source, created_at, updated_at
		FROM product_dimensions
		WHERE gable_product_id = $1`, gableProductID).
		Scan(&d.ID, &d.GableProductID, &d.SKU, &d.LengthIn, &d.WidthIn, &d.HeightIn,
			&d.Stackable, &d.WeightLbsOverride, &d.DefaultSource, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query product_dimension: %w", err)
	}
	return &d, nil
}

// Upsert creates or replaces dimensions for a product.
func (r *Repository) Upsert(ctx context.Context, gableProductID string, in DimensionInput) (*Dimension, error) {
	ex := r.db.GetExecutor(ctx)
	source := in.DefaultSource
	if source == "" {
		source = "MANUAL"
	}
	_, err := ex.Exec(ctx, `
		INSERT INTO product_dimensions
		    (gable_product_id, sku, length_in, width_in, height_in, stackable, weight_lbs_override, default_source)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (gable_product_id) DO UPDATE SET
		    sku = EXCLUDED.sku,
		    length_in = EXCLUDED.length_in,
		    width_in = EXCLUDED.width_in,
		    height_in = EXCLUDED.height_in,
		    stackable = EXCLUDED.stackable,
		    weight_lbs_override = EXCLUDED.weight_lbs_override,
		    default_source = EXCLUDED.default_source,
		    updated_at = NOW()`,
		gableProductID, in.SKU, in.LengthIn, in.WidthIn, in.HeightIn, in.Stackable, in.WeightLbsOverride, source)
	if err != nil {
		return nil, fmt.Errorf("upsert product_dimension: %w", err)
	}
	return r.GetByProductID(ctx, gableProductID)
}
