// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"errors"
	"testing"
	"time"
)

func TestGateReshuffleUnlocked(t *testing.T) {
	// No lock at all.
	if err := gateReshuffle(&Plan{}, false, "", "re-assigning"); err != nil {
		t.Errorf("unlocked plan should permit reshuffle, got %v", err)
	}
	// Explicit lock cleared.
	p := &Plan{Lock: &PlanLock{Locked: false}}
	if err := gateReshuffle(p, false, "", "re-assigning"); err != nil {
		t.Errorf("cleared lock should permit reshuffle, got %v", err)
	}
}

func TestGateReshuffleLockedRefusesWithoutOverride(t *testing.T) {
	p := &Plan{Lock: &PlanLock{Locked: true, Window: LockWindowMorning, LockAt: "06:00"}}
	err := gateReshuffle(p, false, "", "re-assigning trucks")
	if err == nil {
		t.Fatal("locked plan must refuse reshuffle without override")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("expected ErrLocked, got %v", err)
	}
}

func TestGateReshuffleLockedAllowsWithOverride(t *testing.T) {
	p := &Plan{Lock: &PlanLock{Locked: true, Window: LockWindowMorning, LockAt: "06:00"}}
	if err := gateReshuffle(p, true, "Dispatcher A", "re-assigning trucks"); err != nil {
		t.Fatalf("override should permit reshuffle on a locked run, got %v", err)
	}
	if p.Lock.Reason == "" {
		t.Error("expected the override approval to be recorded on the lock reason")
	}
}

func TestApplyLockScheduleAutoLocks(t *testing.T) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")

	// A morning cutoff on a past date has passed → auto-locks.
	past := &Plan{PlanDate: yesterday, Lock: &PlanLock{Window: LockWindowMorning, LockAt: "06:00"}}
	applyLockSchedule(past, time.Now())
	if !past.Lock.Locked {
		t.Error("schedule whose cutoff has passed should auto-lock")
	}

	// A cutoff in the future has not passed → stays unlocked.
	future := &Plan{PlanDate: tomorrow, Lock: &PlanLock{Window: LockWindowAfternoon, LockAt: "11:00"}}
	applyLockSchedule(future, time.Now())
	if future.Lock.Locked {
		t.Error("schedule whose cutoff is in the future should not lock yet")
	}
}
