// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

// Scheduled re-optimization windows + lock states (T2-3).
//
// A run can be locked manually or on a schedule (morning runs lock early,
// afternoon runs later). A locked run is never silently re-shuffled: assign /
// resequence / priority changes need an explicit override (manual approval), and
// a late same-day order is queued for approval instead of reshuffling the run.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/catalog"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// ErrLocked signals a reshuffle was refused because the run is locked. Handlers
// map it to 423 Locked so the UI can prompt for approval.
var ErrLocked = errors.New("plan run is locked")

// parseLockTime resolves a plan-date + HH:MM into a server-local time.
func parseLockTime(planDate, hhmm string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04", planDate+" "+hhmm, time.Local)
}

// applyLockSchedule flips an unlocked run to locked once its scheduled window
// time has passed on the plan date. Pure on the in-memory plan; callers persist
// when appropriate.
func applyLockSchedule(p *Plan, now time.Time) {
	if p.Lock == nil || p.Lock.Locked || p.Lock.LockAt == "" {
		return
	}
	sched, err := parseLockTime(p.PlanDate, p.Lock.LockAt)
	if err != nil {
		return
	}
	if !now.Before(sched) {
		p.Lock.Locked = true
		t := now
		p.Lock.LockedAt = &t
		if p.Lock.Reason == "" {
			p.Lock.Reason = fmt.Sprintf("auto-locked at the %s window cutoff (%s)", strings.ToLower(p.Lock.Window), p.Lock.LockAt)
		}
	}
}

// gateReshuffle refuses a reshuffle on a locked run unless override (manual
// approval) is supplied. It evaluates any scheduled lock first, and records the
// approval on the lock when an override is exercised.
func gateReshuffle(p *Plan, override bool, approvedBy, action string) error {
	applyLockSchedule(p, time.Now())
	if p.Lock == nil || !p.Lock.Locked {
		return nil
	}
	if !override {
		win := p.Lock.Window
		if win == "" {
			win = "manual"
		}
		return fmt.Errorf("%w: %s run locked%s — %s requires manual approval (override)",
			ErrLocked, strings.ToLower(win), lockAtSuffix(p.Lock.LockAt), action)
	}
	who := approvedBy
	if who == "" {
		who = "an approver"
	}
	p.Lock.Reason = fmt.Sprintf("%s approved by %s at %s", action, who, time.Now().Format("15:04"))
	return nil
}

func lockAtSuffix(lockAt string) string {
	if lockAt == "" {
		return ""
	}
	return " (cutoff " + lockAt + ")"
}

// windowLockAt resolves a lock window to its HH:MM cutoff, preferring an explicit
// value, then the configured default for the window.
func (s *Service) windowLockAt(window, explicit string) string {
	if explicit != "" {
		return explicit
	}
	switch strings.ToUpper(window) {
	case LockWindowMorning:
		if s.cfg.LockMorningAt != "" {
			return s.cfg.LockMorningAt
		}
		return "06:00"
	case LockWindowAfternoon:
		if s.cfg.LockAfternoonAt != "" {
			return s.cfg.LockAfternoonAt
		}
		return "11:00"
	default:
		return ""
	}
}

// SetLock sets a run's lock / scheduled-lock state (T2-3).
func (s *Service) SetLock(ctx context.Context, id string, req LockRequest) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	window := strings.ToUpper(strings.TrimSpace(req.Window))
	if window == LockWindowCustom && req.LockAt == "" {
		return nil, fmt.Errorf("lock_at (HH:MM) is required for a CUSTOM window")
	}
	lockAt := req.LockAt
	if window != "" && window != LockWindowCustom {
		lockAt = s.windowLockAt(window, req.LockAt)
	}

	lock := &PlanLock{Window: window, LockAt: lockAt, LockedBy: req.LockedBy, Reason: req.Reason}
	// Omitted `locked` defaults to an immediate lock.
	lockNow := req.Locked == nil || *req.Locked
	if lockNow {
		lock.Locked = true
		now := time.Now()
		lock.LockedAt = &now
		if lock.Reason == "" {
			lock.Reason = "manually locked"
		}
	}
	p.Lock = lock
	// Schedule-only locks auto-engage once the window time has already passed.
	applyLockSchedule(p, time.Now())

	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Unlock clears a run's lock (manual or scheduled) so it can be re-optimized.
func (s *Service) Unlock(ctx context.Context, id, reason, by string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Lock = &PlanLock{Locked: false, LockedBy: by, Reason: reason}
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// AddLateOrder queues a late same-day order onto a run (T2-3). The order is
// pulled from GableLBM, analyzed and appended to the plan so it is visible. When
// the run is locked it is recorded PENDING and NOT routed (approval reshuffles
// it); when unlocked it is recorded APPROVED for the dispatcher to re-assign.
func (s *Service) AddLateOrder(ctx context.Context, id string, req LateAddRequest) (*Plan, error) {
	if req.OrderID == "" {
		return nil, fmt.Errorf("order_id is required")
	}
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	applyLockSchedule(p, time.Now())

	for _, a := range p.Orders {
		if a.OrderID == req.OrderID {
			return nil, fmt.Errorf("order %s is already on this run", req.OrderID)
		}
	}

	orders, err := s.gable.ListOrdersForDate(ctx, p.PlanDate)
	if err != nil {
		return nil, fmt.Errorf("fetch orders: %w", err)
	}
	var found *gable.Order
	for i := range orders {
		if orders[i].ID == req.OrderID {
			found = &orders[i]
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("order %s is not a confirmed GableLBM order for %s", req.OrderID, p.PlanDate)
	}

	products, err := s.catalog.ListEffectiveProducts(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog: %w", err)
	}
	byProduct := make(map[string]catalog.EffectiveProduct, len(products))
	for _, pr := range products {
		byProduct[pr.GableProductID] = pr
	}
	analysis := analyzeOrder(*found, byProduct)
	p.Orders = append(p.Orders, analysis)

	status := LateAddApproved
	if p.Lock != nil && p.Lock.Locked {
		status = LateAddPending
	}
	p.LateAdds = append(p.LateAdds, LateAdd{
		OrderID:      req.OrderID,
		CustomerName: analysis.CustomerName,
		Status:       status,
		RequestedBy:  req.RequestedBy,
		RequestedAt:  time.Now(),
		Note:         req.Note,
	})

	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// ResolveLateAdd approves or rejects a queued late add (T2-3). Approval is the
// manual authorization to reshuffle the locked run, so it re-runs assignment
// (with override) to incorporate the order; rejection drops it from the run.
func (s *Service) ResolveLateAdd(ctx context.Context, id, orderID string, req LateAddApproveRequest) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i := range p.LateAdds {
		if p.LateAdds[i].OrderID == orderID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("no queued late add for order %s", orderID)
	}
	if p.LateAdds[idx].Status != LateAddPending {
		return nil, fmt.Errorf("late add for order %s is already %s", orderID, p.LateAdds[idx].Status)
	}

	if req.Reject {
		p.LateAdds[idx].Status = LateAddRejected
		p.LateAdds[idx].ResolvedBy = req.ApprovedBy
		// Drop the order from the run.
		kept := p.Orders[:0]
		for _, a := range p.Orders {
			if a.OrderID != orderID {
				kept = append(kept, a)
			}
		}
		p.Orders = kept
		if err := s.repo.Update(ctx, p); err != nil {
			return nil, err
		}
		return p, nil
	}

	p.LateAdds[idx].Status = LateAddApproved
	p.LateAdds[idx].ResolvedBy = req.ApprovedBy
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	// Approval authorizes the reshuffle — re-assign with override.
	return s.Assign(ctx, id, true, req.ApprovedBy)
}
