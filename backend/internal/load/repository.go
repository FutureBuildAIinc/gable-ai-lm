// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a load plan does not exist.
var ErrNotFound = errors.New("load plan not found")

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// Save persists a computed plan and returns its assigned id + created_at.
func (r *Repository) Save(ctx context.Context, p *Plan) error {
	ex := r.db.GetExecutor(ctx)

	placements, err := json.Marshal(p.Placements)
	if err != nil {
		return fmt.Errorf("marshal placements: %w", err)
	}
	axleLoads, err := json.Marshal(p.AxleLoads)
	if err != nil {
		return fmt.Errorf("marshal axle_loads: %w", err)
	}

	err = ex.QueryRow(ctx, `
		INSERT INTO load_plans
		    (gable_route_id, gable_delivery_id, gable_vehicle_id, placements,
		     total_weight_lbs, axle_loads, balance_score, gvw_status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, created_at`,
		p.GableRouteID, p.GableDeliveryID, p.GableVehicleID, placements,
		p.TotalWeightLbs, axleLoads, p.BalanceScore, p.GVWStatus).
		Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert load_plan: %w", err)
	}
	return nil
}

// Get returns a stored plan by id.
func (r *Repository) Get(ctx context.Context, id string) (*Plan, error) {
	ex := r.db.GetExecutor(ctx)
	var (
		p          Plan
		placements []byte
		axleLoads  []byte
	)
	err := ex.QueryRow(ctx, `
		SELECT id, gable_route_id, gable_delivery_id, gable_vehicle_id, placements,
		       total_weight_lbs, axle_loads, balance_score, gvw_status, created_at
		FROM load_plans
		WHERE id = $1`, id).
		Scan(&p.ID, &p.GableRouteID, &p.GableDeliveryID, &p.GableVehicleID, &placements,
			&p.TotalWeightLbs, &axleLoads, &p.BalanceScore, &p.GVWStatus, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query load_plan: %w", err)
	}

	if err := json.Unmarshal(placements, &p.Placements); err != nil {
		return nil, fmt.Errorf("unmarshal placements: %w", err)
	}
	if err := json.Unmarshal(axleLoads, &p.AxleLoads); err != nil {
		return nil, fmt.Errorf("unmarshal axle_loads: %w", err)
	}
	return &p, nil
}
