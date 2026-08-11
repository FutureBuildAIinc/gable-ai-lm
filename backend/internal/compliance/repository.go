// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package compliance

import (
	"context"
	"errors"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a restricted point does not exist.
var ErrNotFound = errors.New("restricted point not found")

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

const rpColumns = `id, name, lat, lng, restriction_type, max_gross_weight_lbs,
	max_axle_weight_lbs, max_height_in, notes, created_at, updated_at`

func scanPoint(row pgx.Row) (RestrictedPoint, error) {
	var p RestrictedPoint
	err := row.Scan(&p.ID, &p.Name, &p.Lat, &p.Lng, &p.RestrictionType, &p.MaxGrossWeightLbs,
		&p.MaxAxleWeightLbs, &p.MaxHeightIn, &p.Notes, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// List returns all restricted points.
func (r *Repository) List(ctx context.Context) ([]RestrictedPoint, error) {
	ex := r.db.GetExecutor(ctx)
	rows, err := ex.Query(ctx, `SELECT `+rpColumns+` FROM restricted_points ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query restricted_points: %w", err)
	}
	defer rows.Close()

	var points []RestrictedPoint
	for rows.Next() {
		p, err := scanPoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan restricted_point: %w", err)
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// Create inserts a new restricted point.
func (r *Repository) Create(ctx context.Context, in RestrictedPointInput) (*RestrictedPoint, error) {
	ex := r.db.GetExecutor(ctx)
	rtype := in.RestrictionType
	if rtype == "" {
		rtype = "WEIGHT"
	}
	row := ex.QueryRow(ctx, `
		INSERT INTO restricted_points
		    (name, lat, lng, restriction_type, max_gross_weight_lbs, max_axle_weight_lbs, max_height_in, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+rpColumns,
		in.Name, in.Lat, in.Lng, rtype, in.MaxGrossWeightLbs, in.MaxAxleWeightLbs, in.MaxHeightIn, in.Notes)
	p, err := scanPoint(row)
	if err != nil {
		return nil, fmt.Errorf("insert restricted_point: %w", err)
	}
	return &p, nil
}

// Update replaces a restricted point by id.
func (r *Repository) Update(ctx context.Context, id string, in RestrictedPointInput) (*RestrictedPoint, error) {
	ex := r.db.GetExecutor(ctx)
	rtype := in.RestrictionType
	if rtype == "" {
		rtype = "WEIGHT"
	}
	row := ex.QueryRow(ctx, `
		UPDATE restricted_points SET
		    name = $2, lat = $3, lng = $4, restriction_type = $5,
		    max_gross_weight_lbs = $6, max_axle_weight_lbs = $7, max_height_in = $8,
		    notes = $9, updated_at = NOW()
		WHERE id = $1
		RETURNING `+rpColumns,
		id, in.Name, in.Lat, in.Lng, rtype, in.MaxGrossWeightLbs, in.MaxAxleWeightLbs, in.MaxHeightIn, in.Notes)
	p, err := scanPoint(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update restricted_point: %w", err)
	}
	return &p, nil
}
