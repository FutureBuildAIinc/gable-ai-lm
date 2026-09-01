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

// ErrDateBusy is returned when another push for the same plan date already
// holds that date's serialization lock. NOTHING has been done: no gate has run,
// no route has been sent to GableLBM, no ledger has been written. The caller
// may simply try again.
var ErrDateBusy = errors.New("dispatch date busy")

// dateLockClass namespaces this module's advisory-lock keys.
//
// Postgres advisory locks live in ONE database-wide keyspace, shared by every
// session and every feature that ever takes one. The two-argument form
// pg_try_advisory_xact_lock(classid, objid) is what keeps this module's keys
// from colliding with somebody else's: the class is this constant, and only the
// object id is derived from the date. Without it, any other advisory lock in
// this database that happened to hash to the same bigint would silently
// serialize against dispatch pushes, or be serialized by them.
//
// The value is arbitrary and its only property that matters is that it is
// fixed. Changing it would let an old process and a new one take "the same"
// lock and not contend, so it is a constant and not configuration.
const dateLockClass = 0x4C4D5057 // 'LMPW' — ai-LM push, per workday

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// payload is everything outside the dedicated columns, stored as one JSONB doc.
//
// NOTE, because this has already cost one silent data loss in review: a field
// added to Plan is NOT persisted by being added to Plan. This struct enumerates
// the plan-level fields by hand and marshalPayload/unmarshalPayload copy them
// one by one, so a new Plan field missing from all three vanishes on every
// write. Nothing in the in-memory test suite catches it either — fakePlanStore
// round-trips the whole Plan through encoding/json, so an unregistered field
// survives in every test and is lost only in production. That is what
// TestPayloadRoundTripsTheLiveRouteLedger exists for.
//
// Fields inside Loads/Orders/Stops need no entry here: they ride inside those
// slices and are marshalled wholesale.
type payload struct {
	DepotLat         float64          `json:"depot_lat"`
	DepotLng         float64          `json:"depot_lng"`
	DepotSource      string           `json:"depot_source,omitempty"`
	DepotNote        string           `json:"depot_note,omitempty"`
	Orders           []OrderAnalysis  `json:"orders"`
	Loads            []TruckLoad      `json:"loads"`
	UnassignedOrders []Stop           `json:"unassigned_orders"`
	Lock             *PlanLock        `json:"lock,omitempty"`
	LateAdds         []LateAdd        `json:"late_adds,omitempty"`
	LiveRoutes       []LiveRoute      `json:"live_routes,omitempty"`
	PushedOverrides  []PushedOverride `json:"pushed_overrides,omitempty"`
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
		LiveRoutes:       p.LiveRoutes,
		PushedOverrides:  p.PushedOverrides,
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
	p.LiveRoutes = pl.LiveRoutes
	p.PushedOverrides = pl.PushedOverrides
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

// ListForDate returns EVERY plan for a date, newest first.
//
// It exists because a date legitimately holds more than one plan — a re-ingest
// supersedes rather than replaces — and nothing forces the plan holding live
// routes to be the latest one. GetLatestForDate answers a display question
// ("what is this date's current plan?"); it is not safe to gate on, because a
// dispatcher can push an OLDER plan by id after a newer one exists and the
// latest-only read then reports an empty ledger for a date whose board is live.
//
// A date with no plans is an empty slice, not ErrNotFound: "nothing here" is
// the ordinary first ingest, not a failure.
func (r *Repository) ListForDate(ctx context.Context, date string) ([]*Plan, error) {
	rows, err := r.db.GetExecutor(ctx).Query(ctx, `
		SELECT id, plan_date::text, status, version, payload, created_at, updated_at
		FROM workflow_plans WHERE plan_date=$1
		ORDER BY created_at DESC`, date)
	if err != nil {
		return nil, fmt.Errorf("query workflow_plans by date: %w", err)
	}
	defer rows.Close()

	out := []*Plan{}
	for rows.Next() {
		var p Plan
		var raw []byte
		if err := rows.Scan(&p.ID, &p.PlanDate, &p.Status, &p.Version, &raw, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workflow_plan: %w", err)
		}
		if err := r.unmarshalPayload(raw, &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query workflow_plans by date: %w", err)
	}
	return out, nil
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

// WithDateLock runs fn holding an exclusive lock on one dispatch DATE, so that
// at most one push for that date is ever in flight in this deployment.
//
// The date is the contended resource, not the plan. A date legitimately holds
// several plans and any of them may be pushed; what cannot happen twice at once
// is the read-decide-write cycle over that date's trucks, because GableLBM's
// ReplaceDeliveryRoute is keyed (vehicle_id, scheduled_date) and holds at most
// one non-dispatched route per truck per day. Locking the plan would leave two
// plans for one date racing exactly as before.
//
// The lock is TRANSACTION-SCOPED (pg_try_advisory_xact_lock, not
// pg_advisory_lock). A session-scoped lock is released by an explicit unlock or
// by the session ending, which means a process that is killed mid-push — or
// whose connection is held open by a pooler — can hold a dispatch date shut
// with nobody left to open it. A transaction-scoped lock is released by COMMIT
// or ROLLBACK, and both of those happen when the backend notices the client is
// gone. A crashed process therefore cannot wedge a date, and no lease, sweeper
// or timeout is needed to make that true.
//
// It is a TRY lock, so contention FAILS FAST with ErrDateBusy rather than
// queueing. See Service.Push for that decision and its cost.
//
// fn runs inside the transaction: every repository call it makes picks the
// transaction up from the context (see database.DB.GetExecutor), so the work
// done under the lock commits with the lock's release and is never visible
// half-done to the next holder.
func (r *Repository) WithDateLock(ctx context.Context, date string, fn func(ctx context.Context) error) error {
	return r.db.RunInTx(ctx, func(ctx context.Context) error {
		var acquired bool
		// hashtext() maps the date text into the int4 object-id space. Its
		// exact values are a Postgres implementation detail, which is fine
		// here and would not be if they were stored: the only requirement is
		// that two concurrent sessions on the SAME server agree, and a hash
		// collision between two different dates costs a spurious ErrDateBusy,
		// never a lost mutual exclusion.
		if err := r.db.GetExecutor(ctx).QueryRow(ctx,
			`SELECT pg_try_advisory_xact_lock($1, hashtext($2))`,
			int32(dateLockClass), date).Scan(&acquired); err != nil {
			return fmt.Errorf("take the dispatch-date lock for %s: %w", date, err)
		}
		if !acquired {
			return ErrDateBusy
		}
		return fn(ctx)
	})
}
