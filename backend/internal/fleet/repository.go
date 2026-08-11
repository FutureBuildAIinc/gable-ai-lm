// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a vehicle profile does not exist.
var ErrNotFound = errors.New("vehicle profile not found")

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// List returns all vehicle profiles with their axles.
func (r *Repository) List(ctx context.Context) ([]Profile, error) {
	ex := r.db.GetExecutor(ctx)
	rows, err := ex.Query(ctx, `
		SELECT id, gable_vehicle_id, name, bed_length_in, bed_width_in, bed_height_in,
		       gvwr_lbs, tare_weight_lbs, created_at, updated_at
		FROM vehicle_profiles
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query vehicle_profiles: %w", err)
	}
	defer rows.Close()

	var profiles []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.GableVehicleID, &p.Name, &p.BedLengthIn, &p.BedWidthIn,
			&p.BedHeightIn, &p.GVWRLbs, &p.TareWeightLbs, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan vehicle_profile: %w", err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range profiles {
		axles, err := r.listAxles(ctx, profiles[i].ID)
		if err != nil {
			return nil, err
		}
		profiles[i].Axles = axles
	}
	return profiles, nil
}

// GetByVehicleID returns a single profile by GableLBM vehicle id.
func (r *Repository) GetByVehicleID(ctx context.Context, gableVehicleID string) (*Profile, error) {
	ex := r.db.GetExecutor(ctx)
	var p Profile
	err := ex.QueryRow(ctx, `
		SELECT id, gable_vehicle_id, name, bed_length_in, bed_width_in, bed_height_in,
		       gvwr_lbs, tare_weight_lbs, created_at, updated_at
		FROM vehicle_profiles
		WHERE gable_vehicle_id = $1`, gableVehicleID).
		Scan(&p.ID, &p.GableVehicleID, &p.Name, &p.BedLengthIn, &p.BedWidthIn,
			&p.BedHeightIn, &p.GVWRLbs, &p.TareWeightLbs, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query vehicle_profile: %w", err)
	}

	axles, err := r.listAxles(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	p.Axles = axles
	return &p, nil
}

func (r *Repository) listAxles(ctx context.Context, profileID string) ([]Axle, error) {
	ex := r.db.GetExecutor(ctx)
	rows, err := ex.Query(ctx, `
		SELECT id, axle_number, max_weight_lbs, position_from_front_in, axle_type
		FROM axles
		WHERE vehicle_profile_id = $1
		ORDER BY axle_number`, profileID)
	if err != nil {
		return nil, fmt.Errorf("query axles: %w", err)
	}
	defer rows.Close()

	var axles []Axle
	for rows.Next() {
		var a Axle
		if err := rows.Scan(&a.ID, &a.AxleNumber, &a.MaxWeightLbs, &a.PositionFromFrontIn, &a.AxleType); err != nil {
			return nil, fmt.Errorf("scan axle: %w", err)
		}
		axles = append(axles, a)
	}
	return axles, rows.Err()
}

// Upsert creates or replaces a vehicle profile and its axles atomically.
func (r *Repository) Upsert(ctx context.Context, gableVehicleID string, in ProfileInput) (*Profile, error) {
	err := r.db.RunInTx(ctx, func(ctx context.Context) error {
		ex := r.db.GetExecutor(ctx)
		var profileID string
		err := ex.QueryRow(ctx, `
			INSERT INTO vehicle_profiles
			    (gable_vehicle_id, name, bed_length_in, bed_width_in, bed_height_in, gvwr_lbs, tare_weight_lbs)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (gable_vehicle_id) DO UPDATE SET
			    name = EXCLUDED.name,
			    bed_length_in = EXCLUDED.bed_length_in,
			    bed_width_in = EXCLUDED.bed_width_in,
			    bed_height_in = EXCLUDED.bed_height_in,
			    gvwr_lbs = EXCLUDED.gvwr_lbs,
			    tare_weight_lbs = EXCLUDED.tare_weight_lbs,
			    updated_at = NOW()
			RETURNING id`,
			gableVehicleID, in.Name, in.BedLengthIn, in.BedWidthIn, in.BedHeightIn, in.GVWRLbs, in.TareWeightLbs).
			Scan(&profileID)
		if err != nil {
			return fmt.Errorf("upsert vehicle_profile: %w", err)
		}

		if _, err := ex.Exec(ctx, `DELETE FROM axles WHERE vehicle_profile_id = $1`, profileID); err != nil {
			return fmt.Errorf("clear axles: %w", err)
		}

		for _, a := range in.Axles {
			axleType := a.AxleType
			if axleType == "" {
				axleType = "DRIVE"
			}
			if _, err := ex.Exec(ctx, `
				INSERT INTO axles (vehicle_profile_id, axle_number, max_weight_lbs, position_from_front_in, axle_type)
				VALUES ($1,$2,$3,$4,$5)`,
				profileID, a.AxleNumber, a.MaxWeightLbs, a.PositionFromFrontIn, axleType); err != nil {
				return fmt.Errorf("insert axle: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.GetByVehicleID(ctx, gableVehicleID)
}
