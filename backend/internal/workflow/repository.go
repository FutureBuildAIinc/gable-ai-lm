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

// ErrDateBusy is returned when another writer for the same plan date already
// holds that date. NOTHING has been done: no gate has run, no route has been
// sent to GableLBM, no ledger has been written. The caller may simply try
// again.
//
// Match it with errors.Is. The sentence a dispatcher reads is carried by a
// *DateBusy wrapping it, because "dispatch date busy" is a status, not an
// instruction, and this refusal reaches a human.
var ErrDateBusy = errors.New("dispatch date busy")

// DateBusy is the refusal itself: the sentence written for the dispatcher,
// wrapping the sentinel so handlers and callers can still match on it.
//
// It exists because fmt.Errorf("%w: ...", ErrDateBusy) puts the sentinel's own
// words at the FRONT of everything the operator reads, and "dispatch date
// busy: " is this module talking to itself.
type DateBusy struct{ Msg string }

func (e *DateBusy) Error() string { return e.Msg }
func (e *DateBusy) Unwrap() error { return ErrDateBusy }

// busyf builds the refusal a contended date returns.
func busyf(format string, a ...any) error {
	return &DateBusy{Msg: fmt.Sprintf(format, a...)}
}

type Repository struct {
	db *database.DB
	// hold is the exclusive, time-bounded claim on one dispatch date. See
	// datelease.go for why it is a lease row and not the transaction-scoped
	// advisory lock it replaces.
	hold dateHold
}

func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db, hold: newDateHold(pgDateLeases{db: db})}
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

// WithDateLock runs fn holding an exclusive claim on one dispatch DATE, so that
// at most one writer to that date's dispatch board is ever in flight in this
// deployment.
//
// The date is the contended resource, not the plan. A date legitimately holds
// several plans and any of them may be pushed, re-planned or re-assigned; what
// cannot happen twice at once is the read-decide-write cycle over that date's
// trucks, because GableLBM's ReplaceDeliveryRoute is keyed (vehicle_id,
// scheduled_date) and holds at most one non-dispatched route per truck per day.
// Locking the plan would leave two plans for one date racing exactly as before.
//
// It is a TRY claim, so contention FAILS FAST with ErrDateBusy rather than
// queueing. See Service.Push for that decision and its cost.
//
// The claim is a LEASE ROW, not a transaction-scoped advisory lock, and fn does
// NOT run inside a transaction. That is the whole point: fn makes several
// GableLBM round-trips, and holding a pooled connection across them starved a
// 25-connection pool shared with every other endpoint in the service. Each
// repository call fn makes therefore commits on its own, exactly as it did
// before any of this serialization existed — the claim adds ordering between
// writers, and nothing else. datelease.go has the full argument, including what
// the lease gives up (a dead holder is cleared by expiry rather than by its
// transaction unwinding) and how it buys that back.
func (r *Repository) WithDateLock(ctx context.Context, date string, fn func(ctx context.Context) error) error {
	return r.hold.run(ctx, date, fn)
}
