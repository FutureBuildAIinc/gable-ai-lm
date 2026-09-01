// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// The date hold is the one piece of this serialization whose mistakes are
// SILENT. A hold that is never released shuts a dispatch date until a restart;
// a hold that lets a second writer in lets two writers onto one date and every
// acceptance trial in this package goes on passing, because the double in those
// trials is not this code. So the orchestration — fail fast, watch, stop when
// the hold is gone, release exactly once — is tested here against a substrate in
// memory.
//
// What is NOT tested here is the SQL, or the claim that a session-scoped
// advisory lock on a dedicated connection cannot be starved by pressure on the
// work pool. That is the whole point of the change and it is asserted against a
// real PostgreSQL in datehold_postgres_test.go, which skips without one.

// memHolds is the hold substrate in memory with the same contract as the
// Postgres one: one holder per date, a contender is REFUSED rather than queued,
// and a hold that has ended is definitively not ours.
type memHolds struct {
	mu   sync.Mutex
	held map[string]*memHeld

	acquires, releases int
	acquireErr         error
	// full makes acquire answer as an exhausted hold pool — which is NOT a busy
	// date and must never be reported as one.
	full bool
	// checkErr makes stillOurs unable to answer. That is "I could not ask", and
	// it must not stop the work: the lock is held by the session, not by the
	// check.
	checkErr error
}

type memHeld struct {
	store *memHolds
	date  string
	live  bool
	// checks counts stillOurs calls, so "the hold is watched" is countable.
	checks int
}

func newMemHolds() *memHolds { return &memHolds{held: map[string]*memHeld{}} }

func (m *memHolds) acquire(_ context.Context, date string) (heldDate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquires++
	if m.acquireErr != nil {
		return nil, m.acquireErr
	}
	if m.full {
		return nil, ErrDateHoldsFull
	}
	if cur, ok := m.held[date]; ok && cur.live {
		return nil, nil
	}
	h := &memHeld{store: m, date: date, live: true}
	m.held[date] = h
	return h, nil
}

func (h *memHeld) stillOurs(context.Context) (bool, error) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.checks++
	if h.store.checkErr != nil {
		return false, h.store.checkErr
	}
	return h.live, nil
}

func (h *memHeld) release(context.Context) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.releases++
	if !h.live {
		return
	}
	h.live = false
	if cur, ok := h.store.held[h.date]; ok && cur == h {
		delete(h.store.held, h.date)
	}
}

// steal ends the hold on a date WITHOUT its owner releasing it — the session
// terminated, the machine vanished, a transaction-pooling proxy handed the
// statements to a different backend. Whatever the cause, somebody else may now
// take the date, and the previous owner must stop.
func (m *memHolds) steal(date string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.held[date]; ok {
		h.live = false
		delete(m.held, date)
	}
}

func (m *memHolds) heldNow(date string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.held[date]
	return ok && h.live
}

func (m *memHolds) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acquires, m.releases
}

// testHold is a dateHold with everything scaled to milliseconds so the timing
// properties can be asserted rather than described.
func testHold(h dateHoldStore) dateHold {
	return dateHold{holds: h, checkEvery: 15 * time.Millisecond, checkWait: time.Second, ceiling: 300 * time.Millisecond, releaseWait: time.Second}
}

// TestASecondHolderOfTheSameDateIsRefusedHavingRunNothing is the exclusion, and
// the fail-fast decision underneath it.
func TestASecondHolderOfTheSameDateIsRefusedHavingRunNothing(t *testing.T) {
	l := newMemHolds()
	h := testHold(l)

	inner := 0
	err := h.run(context.Background(), "2026-06-26", func(context.Context) error {
		innerErr := h.run(context.Background(), "2026-06-26", func(context.Context) error {
			inner++
			return nil
		})
		if !errors.Is(innerErr, ErrDateBusy) {
			t.Errorf("a second holder must be refused with ErrDateBusy, got %v", innerErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the first holder: %v", err)
	}
	if inner != 0 {
		t.Fatalf("the refused holder ran its work %d time(s) — a fail-fast claim that has already run is not fail-fast", inner)
	}
	if l.heldNow("2026-06-26") {
		t.Error("the date is still held after the work finished")
	}
}

// TestAdjacentDatesDoNotContend: the claim is per DATE, and tomorrow is not
// today. A claim that serialized every date would serialize the whole service.
func TestAdjacentDatesDoNotContend(t *testing.T) {
	l := newMemHolds()
	h := testHold(l)
	ran := false
	err := h.run(context.Background(), "2026-06-26", func(context.Context) error {
		return h.run(context.Background(), "2026-06-27", func(context.Context) error {
			ran = true
			return nil
		})
	})
	if err != nil {
		t.Fatalf("two different dates must both be holdable: %v", err)
	}
	if !ran {
		t.Fatal("the second date's work never ran")
	}
}

// TestTheDateIsReleasedWhateverTheWorkReturns. A date left held by a failed
// operation is a dispatch day nobody can write until a process restarts.
func TestTheDateIsReleasedWhateverTheWorkReturns(t *testing.T) {
	boom := errors.New("the ERP refused")
	for _, tc := range []struct {
		name string
		work func(context.Context) error
	}{
		{"work succeeded", func(context.Context) error { return nil }},
		{"work failed", func(context.Context) error { return boom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newMemHolds()
			_ = testHold(l).run(context.Background(), "2026-06-26", tc.work)
			if l.heldNow("2026-06-26") {
				t.Error("the date is still held")
			}
			if _, releases := l.counts(); releases != 1 {
				t.Errorf("release ran %d time(s), want exactly 1", releases)
			}
		})
	}
}

// TestAReleasedHoldCannotTouchTheNextHolder is the property the lease's
// per-acquisition holder token used to carry, restated for a substrate that has
// no tokens.
//
// A hold is a thing, not a name: once released it refers to nothing, so a stale
// reference to it cannot release, extend or invalidate the hold somebody else
// has since taken on the same date. Under the lease this had to be enforced by
// conditioning every statement on a token; here it is enforced by the hold
// being a distinct object, and this test is what says so out loud.
func TestAReleasedHoldCannotTouchTheNextHolder(t *testing.T) {
	l := newMemHolds()
	first, err := l.acquire(context.Background(), "2026-06-26")
	if err != nil || first == nil {
		t.Fatalf("first acquire: %v", err)
	}
	first.release(context.Background())

	second, err := l.acquire(context.Background(), "2026-06-26")
	if err != nil || second == nil {
		t.Fatalf("second acquire: %v", err)
	}
	// The stale reference tries everything it can still do.
	first.release(context.Background())
	if ours, err := second.stillOurs(context.Background()); err != nil || !ours {
		t.Fatalf("the live holder lost its date to a released one's release (ours=%v err=%v)", ours, err)
	}
	if !l.heldNow("2026-06-26") {
		t.Fatal("the date is free while a holder still believes it owns it")
	}
	second.release(context.Background())
}

// TestAHoldWhoseSessionEndedIsReclaimable is what replaces the lease's expiry.
//
// The lease freed a crashed holder's date by a CLOCK: wait out the TTL. A
// session lock frees it by a FACT: the session ended, so the lock is gone, and
// the next writer takes the date immediately rather than after a TTL of a
// dispatcher staring at a refusal.
func TestAHoldWhoseSessionEndedIsReclaimable(t *testing.T) {
	l := newMemHolds()
	if _, err := l.acquire(context.Background(), "2026-06-26"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got, _ := l.acquire(context.Background(), "2026-06-26"); got != nil {
		t.Fatal("a live hold was handed out twice")
	}
	l.steal("2026-06-26") // the holder's process died

	ran := false
	if err := testHold(l).run(context.Background(), "2026-06-26", func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("a date whose holder's session ended must be reclaimable: %v", err)
	}
	if !ran {
		t.Fatal("the work never ran")
	}
}

// TestALongHoldSurvivesWithoutRenewal is the defect this substrate exists to
// close, stated on the orchestration.
//
// The lease had to be RENEWED, so a holder that could not get a connection lost
// a date it was still using. Nothing renews here: the lock is held by a session
// that is already ours. A hold that lasts many check periods must therefore
// still be ours at the end of them, and the checks must have HAPPENED — a
// watcher that never ran would satisfy this test by doing nothing.
func TestALongHoldSurvivesWithoutRenewal(t *testing.T) {
	l := newMemHolds()
	h := testHold(l) // checks every 15ms
	var held *memHeld
	err := h.run(context.Background(), "2026-06-26", func(work context.Context) error {
		l.mu.Lock()
		held = l.held["2026-06-26"]
		l.mu.Unlock()
		select {
		case <-time.After(120 * time.Millisecond):
		case <-work.Done():
			return work.Err()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a hold held across many check periods was lost: %v", err)
	}
	l.mu.Lock()
	checks := held.checks
	l.mu.Unlock()
	if checks == 0 {
		t.Fatal("the hold was never checked — this test would pass against a watcher that does not exist")
	}
}

// TestACheckThatCannotBeTakenDoesNotStopTheWork.
//
// This is the difference the lease could not express. A renewal that failed was
// indistinguishable from a lease that was gone, because the lease's life
// DEPENDED on the renewal. Here the lock is held by the session and the check
// keeps nothing, so "I could not ask" is not "the answer is no" — and treating
// it as no would abort live pushes every time the database hiccuped.
func TestACheckThatCannotBeTakenDoesNotStopTheWork(t *testing.T) {
	l := newMemHolds()
	l.checkErr = errors.New("read tcp: i/o timeout")
	err := testHold(l).run(context.Background(), "2026-06-26", func(work context.Context) error {
		select {
		case <-time.After(100 * time.Millisecond):
			return nil
		case <-work.Done():
			return errors.New("the work was stopped by a check that merely failed to answer")
		}
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// TestLosingTheHoldStopsTheWork is the whole reason the watcher exists.
//
// If the hold is gone, somebody else is entitled to write this date, and going
// on would be exactly the two-writer race the claim exists to prevent.
func TestLosingTheHoldStopsTheWork(t *testing.T) {
	l := newMemHolds()
	stopped := make(chan struct{})
	err := testHold(l).run(context.Background(), "2026-06-26", func(work context.Context) error {
		l.steal("2026-06-26")
		select {
		case <-work.Done():
			close(stopped)
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("the work was never stopped")
		}
	})
	select {
	case <-stopped:
	default:
		t.Fatal("the work ran on after the hold was gone")
	}
	if !errors.Is(err, ErrDateHoldLost) {
		t.Fatalf("a hold lost mid-flight must be reported as ErrDateHoldLost, got %v", err)
	}
	if errors.Is(err, ErrDateBusy) {
		t.Fatal("a hold lost MID-FLIGHT was reported as a busy date — that tells a dispatcher nothing happened about a push that may already have put trucks on the board")
	}
}

// TestTheWorksOwnAccountSurvivesALostHold. Losing the hold cancels the work, so
// the work usually has its own account of where it got to — "two trucks are
// live" — and that account is what the dispatcher must act on.
func TestTheWorksOwnAccountSurvivesALostHold(t *testing.T) {
	l := newMemHolds()
	partial := errors.New("push stopped after truck 2 of 4; two routes are live on the board")
	err := testHold(l).run(context.Background(), "2026-06-26", func(work context.Context) error {
		l.steal("2026-06-26")
		<-work.Done()
		return partial
	})
	if !errors.Is(err, partial) {
		t.Fatalf("the work's own account was replaced by plumbing: %v", err)
	}
}

// TestAHoldHasACeiling. A process that is alive but wedged — an ERP that accepts
// the connection and never answers — must not hold a dispatch date for ever.
// Under a session lock there is no TTL to fall back on, so the ceiling is the
// ONLY bound and it is load-bearing rather than belt-and-braces.
func TestAHoldHasACeiling(t *testing.T) {
	l := newMemHolds()
	h := testHold(l) // ceiling 300ms
	start := time.Now()
	err := h.run(context.Background(), "2026-06-26", func(work context.Context) error {
		<-work.Done()
		return work.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the work must be stopped by the ceiling, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the ceiling took %s to fire", elapsed)
	}
	if l.heldNow("2026-06-26") {
		t.Error("the date is still held after the ceiling fired")
	}
}

// TestProductionHoldTimingsAreCoherent guards the constants against an edit that
// looks harmless.
func TestProductionHoldTimingsAreCoherent(t *testing.T) {
	if dateHoldCheck >= dateHoldCeiling {
		t.Errorf("a hold (%s) that ends before it is ever checked (%s) is never watched at all", dateHoldCeiling, dateHoldCheck)
	}
	if dateHoldWait >= dateHoldCheck {
		t.Errorf("one check may block for %s out of a %s period, so the watcher spends its life in a single check", dateHoldWait, dateHoldCheck)
	}
	if dateHoldWait > 5*time.Second {
		t.Errorf("a contended writer waits %s before being refused — contention is supposed to cost a fast 409, not a parked request", dateHoldWait)
	}
	if dateHoldCeiling < time.Minute {
		t.Errorf("a ceiling of %s cuts off legitimate multi-truck pushes with 15s-per-truck ERP round-trips", dateHoldCeiling)
	}
	if dateHoldRelease <= 0 {
		t.Error("release needs its own budget: it runs when the work's context is already done")
	}
}

// TestAnUnavailableHoldSubstrateRefusesRatherThanProceeds is the failure
// direction. If the hold cannot be taken, the ONLY safe answer is to do nothing.
// The error is also not ErrDateBusy — "the database is unreachable" and
// "somebody else is pushing" need different answers from an operator.
func TestAnUnavailableHoldSubstrateRefusesRatherThanProceeds(t *testing.T) {
	l := newMemHolds()
	l.acquireErr = errors.New("dial tcp: connection refused")
	ran := false
	err := testHold(l).run(context.Background(), "2026-06-26", func(context.Context) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("the work ran without a hold on the date")
	}
	if err == nil {
		t.Fatal("an unavailable hold substrate must be reported")
	}
	if errors.Is(err, ErrDateBusy) {
		t.Fatalf("an unreachable database must not be reported as a busy date: %v", err)
	}
}

// TestAFullHoldPoolIsNotReportedAsABusyDate.
//
// The dedicated pool is the price of the substrate, and running out of it is a
// real refusal — but it is not "somebody else is writing 26 June". Collapsing
// the two would put a sentence naming another dispatcher in front of a user
// nobody is competing with, and would make the hold pool's size invisible in
// exactly the incident where it is the answer.
func TestAFullHoldPoolIsNotReportedAsABusyDate(t *testing.T) {
	l := newMemHolds()
	l.full = true
	ran := false
	err := testHold(l).run(context.Background(), "2026-06-26", func(context.Context) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("the work ran without a hold on the date")
	}
	if !errors.Is(err, ErrDateHoldsFull) {
		t.Fatalf("want ErrDateHoldsFull, got %v", err)
	}
	if errors.Is(err, ErrDateBusy) {
		t.Fatal("an exhausted hold pool was reported as a busy date")
	}
}

// TestConcurrentHoldersOfOneDateNeverOverlap is the property itself, asserted on
// the orchestration rather than through the HTTP surface: many goroutines, one
// date, and never two inside at once.
func TestConcurrentHoldersOfOneDateNeverOverlap(t *testing.T) {
	l := newMemHolds()
	h := testHold(l)

	var mu sync.Mutex
	inside, maxInside, granted := 0, 0, 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = h.run(context.Background(), "2026-06-26", func(context.Context) error {
				mu.Lock()
				inside++
				granted++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	close(start)
	wg.Wait()

	if maxInside != 1 {
		t.Fatalf("%d holders were inside the date at once", maxInside)
	}
	if granted == 0 {
		t.Fatal("nobody ever got the date")
	}
	if granted == 24 {
		t.Fatal("all 24 holders got the date without one refusal — this run never contended, so its exclusion proves nothing")
	}
}

// TestTheDateHoldDoesNotRunItsWorkInsideATransaction is the availability
// property stated against the real Repository rather than against a double.
//
// This is what regressed once and no test could see. The first version of this
// serialization took pg_try_advisory_xact_lock, which is released by COMMIT — so
// holding a date meant holding an open transaction, and an open transaction
// means one of MaxConns=25 pooled connections pinned across every GableLBM
// round-trip inside the hold, up to one 15s ERP timeout per truck. Since the
// hold deliberately does NOT serialize different dates against each other,
// twenty-five slow pushes on twenty-five different days could take every
// connection in the pool and stall every endpoint in the service.
//
// The assertion is exact: whatever executor the work sees must be the POOL, not
// a transaction. A future edit that reintroduces RunInTx around the hold fails
// here and nowhere else.
func TestTheDateHoldDoesNotRunItsWorkInsideATransaction(t *testing.T) {
	db := &database.DB{} // deliberately pool-less: opening a transaction would need one
	r := &Repository{db: db, hold: testHold(newMemHolds())}

	ran := false
	err := r.WithDateLock(context.Background(), "2026-06-26", func(ctx context.Context) error {
		ran = true
		if tx, isTx := db.GetExecutor(ctx).(pgx.Tx); isTx {
			t.Errorf("the work runs inside transaction %v — it holds a pooled connection across every ERP round-trip it makes", tx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithDateLock: %v", err)
	}
	if !ran {
		t.Fatal("the work never ran")
	}
}

// TestTheHoldNeverDrawsOnTheWorkPool is the fix stated structurally, so it holds
// for reasons a reader can check rather than for reasons a load test observed.
//
// NewRepository is handed two databases and the hold must be built from the
// SECOND. Passing the work pool to the hold is precisely the coupling that let
// pool pressure hand one dispatch date to two writers, and it is a one-word edit
// away at all times.
func TestTheHoldNeverDrawsOnTheWorkPool(t *testing.T) {
	work := &database.DB{}
	holds := &database.DB{}
	r := NewRepository(work, holds)

	pg, ok := r.hold.holds.(pgDateHolds)
	if !ok {
		t.Fatalf("the production hold is a %T, not the Postgres one", r.hold.holds)
	}
	if pg.pool != holds.Pool {
		t.Error("the hold was not built from the hold pool")
	}
	if r.db != work {
		t.Error("the plan store was not built from the work pool")
	}
	// Both are nil here, which is why the check above cannot stand alone: the
	// wiring is asserted again where the pools are real, in
	// TestTheHoldPoolIsSeparateFromTheWorkPool (datehold_postgres_test.go).
	if work.Pool != nil || holds.Pool != nil {
		t.Fatal("this test's premise changed: it compares nil pools by identity")
	}
}

// TestTheHoldPoolIsBuiltForHoldsAndNotForWork pins the pool settings that make
// the substrate safe, each of which is invisible until the day it is wrong.
func TestTheHoldPoolIsBuiltForHoldsAndNotForWork(t *testing.T) {
	pc := DateHoldPoolConfig(0)
	if pc.MaxConns != defaultDateHoldConns {
		t.Errorf("MaxConns %d, want the default %d", pc.MaxConns, defaultDateHoldConns)
	}
	if pc.MinConns != pc.MaxConns {
		t.Errorf("MinConns %d != MaxConns %d — a hold pool that has to DIAL under pressure depends on the very thing it protects against",
			pc.MinConns, pc.MaxConns)
	}
	if pc.StatementTimeout <= 0 || pc.StatementTimeout > 5*time.Second {
		t.Errorf("statement timeout %s — the three statements a hold runs are a lock, an unlock and an EXISTS", pc.StatementTimeout)
	}
	for _, k := range []string{"tcp_keepalives_idle", "tcp_keepalives_interval", "tcp_keepalives_count"} {
		if pc.Extra[k] == "" {
			t.Errorf("%s is unset: a session lock is released when its session ends, so a machine that vanishes without closing its sockets holds a dispatch date until the OS default expires", k)
		}
	}
	if pc.Extra["application_name"] == "" {
		t.Error("the hold connections must name themselves: 'what is holding this date' is the first question in an incident, and the lease row that used to answer it is gone")
	}
	if got := DateHoldPoolConfig(3).MaxConns; got != 3 {
		t.Errorf("an explicit size was ignored: MaxConns=%d", got)
	}
}

// ---------------------------------------------------------------------------
// the sentences and the re-entrancy, at the service seam
// ---------------------------------------------------------------------------

// holdFailStore is a plan store whose date claim always answers with a chosen
// error, so the sentences holdDate builds out of them can be read without a
// Postgres. Everything else is the ordinary in-memory store.
type holdFailStore struct {
	*fakePlanStore
	err error
}

func (s holdFailStore) WithDateLock(context.Context, string, func(context.Context) error) error {
	return s.err
}

// TestAFullHoldPoolTellsTheDispatcherSomethingTrue.
//
// Exhausting the hold pool is a refusal a dispatcher can act on, but it is NOT
// "somebody else is writing this date" and must not be dressed as one: the
// sentence would name a rival who does not exist, and the operator chasing the
// wrong cause would never find the setting that is actually the answer. What it
// shares with a busy date — nothing happened, repeat — is what it keeps.
func TestAFullHoldPoolTellsTheDispatcherSomethingTrue(t *testing.T) {
	svc := newTestService(holdFailStore{newFakePlanStore(), ErrDateHoldsFull}, fleetOf("v1"), Config{})
	ran := false
	err := svc.holdDate(context.Background(), "2026-06-26", "pushed", "this push did not run", func(context.Context) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("the work ran without a claim on the date")
	}
	if !errors.Is(err, ErrDateBusy) {
		t.Fatalf("the refusal must still reach the client as a retryable 409: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"2026-06-26", "this push did not run", "Nothing was sent to GableLBM", "try again"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must tell the dispatcher %q; got %q", want, msg)
		}
	}
	if strings.Contains(msg, "by someone else") {
		t.Errorf("an exhausted hold pool blamed a dispatcher who does not exist: %q", msg)
	}
}

// TestTheClaimIsReenteredOnlyForTheDateItWasTakenOn.
//
// Re-entrancy exists so that approving a late add — one action to a dispatcher,
// two writers to this package — does not refuse itself. It must be keyed on the
// DATE, because a call chain holding 26 June has no claim whatsoever on 27 June,
// and passing straight through for it would run an unserialized writer while
// believing it was covered.
func TestTheClaimIsReenteredOnlyForTheDateItWasTakenOn(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, fleetOf("v1"), Config{})
	inner, other := 0, 0
	err := svc.holdDate(context.Background(), "2026-06-26", "changed", "nothing happened", func(ctx context.Context) error {
		if err := svc.holdDate(ctx, "2026-06-26", "changed", "nothing happened", func(context.Context) error {
			inner++
			return nil
		}); err != nil {
			return err
		}
		return svc.holdDate(ctx, "2026-06-27", "changed", "nothing happened", func(context.Context) error {
			other++
			return nil
		})
	})
	if err != nil {
		t.Fatalf("holdDate: %v", err)
	}
	if inner != 1 || other != 1 {
		t.Fatalf("inner work ran %d time(s) and the other date's %d — both must run once", inner, other)
	}
	grants, refusals := store.lockCounts()
	if refusals != 0 {
		t.Errorf("the operation refused itself %d time(s) — a claim that cannot be re-entered makes approving a late add impossible", refusals)
	}
	if grants != 2 {
		t.Errorf("%d claims were taken, want 2: one for 26 June (re-entered once) and one for 27 June, which this call chain does not hold", grants)
	}
}
