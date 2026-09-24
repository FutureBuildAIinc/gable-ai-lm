// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a route plan does not exist.
var ErrNotFound = errors.New("route plan not found")

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// Save persists a draft plan and assigns its id/timestamps.
func (r *Repository) Save(ctx context.Context, p *Plan) error {
	ex := r.db.GetExecutor(ctx)
	stops, err := json.Marshal(p.Stops)
	if err != nil {
		return fmt.Errorf("marshal stops: %w", err)
	}
	loads, err := json.Marshal(p.Loads)
	if err != nil {
		return fmt.Errorf("marshal loads: %w", err)
	}
	err = ex.QueryRow(ctx, `
		INSERT INTO route_plans
		    (plan_date, gable_branch_id, gable_vehicle_id, stops, loads, total_distance_mi, total_duration_min, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, created_at, updated_at`,
		p.PlanDate, p.GableBranchID, p.GableVehicleID, stops, loads, p.TotalDistanceMi, p.TotalDurationMin, p.Status).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert route_plan: %w", err)
	}
	return nil
}

// Get returns a stored plan by id.
func (r *Repository) Get(ctx context.Context, id string) (*Plan, error) {
	ex := r.db.GetExecutor(ctx)
	var (
		p     Plan
		stops []byte
		loads []byte
	)
	err := ex.QueryRow(ctx, `
		SELECT id, plan_date::text, gable_branch_id, gable_vehicle_id, stops, loads,
		       total_distance_mi, total_duration_min, status, created_at, updated_at
		FROM route_plans WHERE id = $1`, id).
		Scan(&p.ID, &p.PlanDate, &p.GableBranchID, &p.GableVehicleID, &stops, &loads,
			&p.TotalDistanceMi, &p.TotalDurationMin, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query route_plan: %w", err)
	}
	if err := json.Unmarshal(stops, &p.Stops); err != nil {
		return nil, fmt.Errorf("unmarshal stops: %w", err)
	}
	if err := json.Unmarshal(loads, &p.Loads); err != nil {
		return nil, fmt.Errorf("unmarshal loads: %w", err)
	}
	// UnassignedStops is computed in-memory during Plan() and not persisted, so
	// it is nil on read. Normalize to an empty slice so the API serializes it as
	// [] rather than null (which crashes clients that read .length).
	if p.UnassignedStops == nil {
		p.UnassignedStops = []Stop{}
	}
	// DepotSource/DepotNote are likewise not persisted — route_plans has no
	// column for either — and so come back as "" here. They are left as "" on
	// purpose: re-resolving the origin now would answer today's question about
	// the day this plan was built for, and any invented value would read as
	// authoritative provenance that nobody recorded. An empty note on this path
	// means "not stored", not "no fallback happened"; see Plan.DepotSource. A
	// migration adding the two columns is what would change this, and it would
	// change it here, in the SELECT.
	return &p, nil
}

// UpdateStatus sets a plan's status (e.g. DRAFT → APPROVED).
func (r *Repository) UpdateStatus(ctx context.Context, id, status string) error {
	ex := r.db.GetExecutor(ctx)
	tag, err := ex.Exec(ctx, `UPDATE route_plans SET status = $2, updated_at = NOW() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("update route_plan status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
