// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a workflow plan does not exist.
var ErrNotFound = errors.New("workflow plan not found")

// ErrVersionConflict is returned when a plan was modified by someone else
// between the read and the write of a read-modify-write cycle. The caller's
// change was NOT applied; the handler maps this to 409 Conflict so the UI can
// reload the current plan and retry. Two ordinary actors on one plan (a
// dispatcher rerouting while a yard lead signs off) hit this path, and each
// net/http request runs on its own goroutine, so it applies at INSTANCE_COUNT=1.
var ErrVersionConflict = errors.New("workflow plan was modified concurrently — reload and retry")

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// payload is everything outside the dedicated columns, stored as one JSONB doc.
type payload struct {
	DepotLat         float64         `json:"depot_lat"`
	DepotLng         float64         `json:"depot_lng"`
	DepotSource      string          `json:"depot_source,omitempty"`
	DepotNote        string          `json:"depot_note,omitempty"`
	Orders           []OrderAnalysis `json:"orders"`
	Loads            []TruckLoad     `json:"loads"`
	UnassignedOrders []Stop          `json:"unassigned_orders"`
	Lock             *PlanLock       `json:"lock,omitempty"`
	LateAdds         []LateAdd       `json:"late_adds,omitempty"`
}

func (r *Repository) marshalPayload(p *Plan) ([]byte, error) {
	return json.Marshal(payload{
		DepotLat:         p.DepotLat,
		DepotLng:         p.DepotLng,
		DepotSource:      p.DepotSource,
		DepotNote:        p.DepotNote,
		Orders:           p.Orders,
		Loads:            p.Loads,
		UnassignedOrders: p.UnassignedOrders,
		Lock:             p.Lock,
		LateAdds:         p.LateAdds,
	})
}

func (r *Repository) unmarshalPayload(raw []byte, p *Plan) error {
	var pl payload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return fmt.Errorf("unmarshal workflow payload: %w", err)
	}
	p.DepotLat = pl.DepotLat
	p.DepotLng = pl.DepotLng
	p.DepotSource = pl.DepotSource
	p.DepotNote = pl.DepotNote
	p.Orders = pl.Orders
	p.Loads = pl.Loads
	p.UnassignedOrders = pl.UnassignedOrders
	p.Lock = pl.Lock
	p.LateAdds = pl.LateAdds
	if p.Orders == nil {
		p.Orders = []OrderAnalysis{}
	}
	if p.Loads == nil {
		p.Loads = []TruckLoad{}
	}
	if p.UnassignedOrders == nil {
		p.UnassignedOrders = []Stop{}
	}
	return nil
}

// Create inserts a new plan and assigns id/timestamps.
func (r *Repository) Create(ctx context.Context, p *Plan) error {
	raw, err := r.marshalPayload(p)
	if err != nil {
		return fmt.Errorf("marshal workflow payload: %w", err)
	}
	err = r.db.GetExecutor(ctx).QueryRow(ctx, `
		INSERT INTO workflow_plans (plan_date, status, payload)
		VALUES ($1, $2, $3)
		RETURNING id, version, created_at, updated_at`,
		p.PlanDate, p.Status, raw).
		Scan(&p.ID, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert workflow_plan: %w", err)
	}
	return nil
}

// Update persists the current state of an existing plan under optimistic
// concurrency: the row is only written when its stored version still matches
// the one this caller read (p.Version), and the write bumps it. A concurrent
// actor who already saved makes this statement match zero rows, so the caller
// is told ErrVersionConflict instead of silently overwriting the other change.
// On success p.Version is advanced so the returned plan is immediately usable.
func (r *Repository) Update(ctx context.Context, p *Plan) error {
	raw, err := r.marshalPayload(p)
	if err != nil {
		return fmt.Errorf("marshal workflow payload: %w", err)
	}
	var next int
	err = r.db.GetExecutor(ctx).QueryRow(ctx, `
		UPDATE workflow_plans
		SET status=$2, payload=$3, version=version+1, updated_at=NOW()
		WHERE id=$1 AND version=$4
		RETURNING version`,
		p.ID, p.Status, raw, p.Version).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row matched: either the plan is gone, or someone else wrote it
		// first. Distinguish the two so the handler can answer 404 vs 409.
		var exists bool
		if qerr := r.db.GetExecutor(ctx).QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM workflow_plans WHERE id=$1)`, p.ID).Scan(&exists); qerr != nil {
			return fmt.Errorf("update workflow_plan (conflict check): %w", qerr)
		}
		if !exists {
			return ErrNotFound
		}
		return ErrVersionConflict
	}
	if err != nil {
		return fmt.Errorf("update workflow_plan: %w", err)
	}
	p.Version = next
	return nil
}

// Get returns one plan by id.
func (r *Repository) Get(ctx context.Context, id string) (*Plan, error) {
	var p Plan
	var raw []byte
	err := r.db.GetExecutor(ctx).QueryRow(ctx, `
		SELECT id, plan_date::text, status, version, payload, created_at, updated_at
		FROM workflow_plans WHERE id=$1`, id).
		Scan(&p.ID, &p.PlanDate, &p.Status, &p.Version, &raw, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query workflow_plan: %w", err)
	}
	if err := r.unmarshalPayload(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetLatestForDate returns the most recent plan for a date, or ErrNotFound.
func (r *Repository) GetLatestForDate(ctx context.Context, date string) (*Plan, error) {
	var p Plan
	var raw []byte
	err := r.db.GetExecutor(ctx).QueryRow(ctx, `
		SELECT id, plan_date::text, status, version, payload, created_at, updated_at
		FROM workflow_plans WHERE plan_date=$1
		ORDER BY created_at DESC LIMIT 1`, date).
		Scan(&p.ID, &p.PlanDate, &p.Status, &p.Version, &raw, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query workflow_plan by date: %w", err)
	}
	if err := r.unmarshalPayload(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
