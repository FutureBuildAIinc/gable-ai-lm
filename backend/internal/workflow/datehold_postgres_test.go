// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// Everything the in-memory suite cannot say.
//
// datehold_test.go tests the ORCHESTRATION against a double. The claim this
// commit actually makes is about the SUBSTRATE — that a session-scoped advisory
// lock held on a dedicated connection cannot be taken away from a live holder by
// pressure on the pool its work uses — and no double can answer that, because
// the double is not the thing that failed. The lease it replaces passed every
// test in that file while handing one dispatch date to two writers against a
// real PostgreSQL.
//
// So these run against a real one, or they skip. They are named for the two
// reproductions that condemned the lease:
//
//	E4  one pool, every connection parked for 28s in queries well inside the
//	    deployment's own 30s statement_timeout. Legal traffic, no fault. Under
//	    the lease a second writer ACQUIRED the same date at t=30.03s while the
//	    first was still writing it; peak concurrent holders 2.
//	E5  two instances, two pools, Postgres idle, no crash and no partition. Only
//	    instance-1's pool oversubscribed by ordinary legal traffic. Under the
//	    lease instance-2 held the date from t=30.06s to t=32.06s while instance-1
//	    wrote on until t=38.03s; peak concurrent holders 2.
//
// Run them with:
//
//	AILM_TEST_DATABASE_URL=postgres://... go test ./internal/workflow/ -run Postgres

const testHoldConns = 4

// pgTest dials a work pool and a hold pool against the same database, exactly as
// cmd/server does.
func pgTest(t *testing.T, workConns int32) (work *database.DB, holds *database.DB) {
	t.Helper()
	return pgTestWithHolds(t, workConns, testHoldConns)
}

func pgTestWithHolds(t *testing.T, workConns, holdConns int32) (work *database.DB, holds *database.DB) {
	t.Helper()
	url := os.Getenv("AILM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AILM_TEST_DATABASE_URL is unset: the substrate claim in datehold.go is about PostgreSQL and cannot be tested without one")
	}
	wc := database.DefaultPoolConfig()
	wc.MaxConns, wc.MinConns = workConns, workConns
	var err error
	if work, err = database.Connect(url, wc); err != nil {
		t.Fatalf("dial the work pool: %v", err)
	}
	t.Cleanup(work.Close)
	if holds, err = database.Connect(url, DateHoldPoolConfig(holdConns)); err != nil {
		t.Fatalf("dial the hold pool: %v", err)
	}
	t.Cleanup(holds.Close)
	return work, holds
}

// pgHold builds the production hold on the hold pool.
func pgHold(holds *database.DB) dateHold {
	return newDateHold(pgDateHolds{pool: holds.Pool})
}

// adminConn is a connection OUTSIDE both pools, used to observe them.
func adminConn(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), os.Getenv("AILM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func testDate() string {
	return fmt.Sprintf("2026-%02d-%02d", time.Now().UnixNano()%12+1, time.Now().UnixNano()/13%28+1)
}

// occupancy is the observer the reproductions are measured with: who is inside
// the date's work right now, and what was the most at once.
type occupancy struct {
	mu    sync.Mutex
	t0    time.Time
	in    int
	peak  int
	lines []string
}

func newOccupancy() *occupancy { return &occupancy{t0: time.Now()} }

func (o *occupancy) log(f string, a ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lines = append(o.lines, fmt.Sprintf("t=%6.2fs  %s", time.Since(o.t0).Seconds(), fmt.Sprintf(f, a...)))
}

func (o *occupancy) enter(who string) {
	o.mu.Lock()
	o.in++
	if o.in > o.peak {
		o.peak = o.in
	}
	n := o.in
	o.mu.Unlock()
	o.log("%s ENTERED the date  (concurrent holders now %d)", who, n)
}

func (o *occupancy) leave(who string) {
	o.mu.Lock()
	o.in--
	n := o.in
	o.mu.Unlock()
	o.log("%s left the date     (concurrent holders now %d)", who, n)
}

func (o *occupancy) report(t *testing.T) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, l := range o.lines {
		t.Log(l)
	}
	t.Logf("PEAK CONCURRENT HOLDERS OF ONE DATE = %d", o.peak)
	return o.peak
}

// holder is the writer under test: it takes the date and works for up to d,
// making the ordinary repository round-trips a real operation makes, and stops
// the moment its hold ends.
func holder(ctx context.Context, o *occupancy, h dateHold, work *database.DB, date, who string, d time.Duration) error {
	return h.run(ctx, date, func(wctx context.Context) error {
		o.enter(who)
		defer o.leave(who)
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			select {
			case <-wctx.Done():
				return wctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			// One small write against the WORK pool, like the ledger writes a
			// push makes between ERP round-trips. It queues behind the pressure
			// exactly as it would in production; the hold must not.
			wq, cancel := context.WithTimeout(wctx, 30*time.Second)
			_, _ = work.Pool.Exec(wq, `SELECT 1`)
			cancel()
		}
		return nil
	})
}

// contender polls for the same date until it gets in or the deadline passes.
func contender(ctx context.Context, o *occupancy, h dateHold, date, who string, until time.Duration) {
	deadline := time.Now().Add(until)
	for time.Now().Before(deadline) {
		err := h.run(ctx, date, func(context.Context) error {
			o.enter(who)
			defer o.leave(who)
			time.Sleep(2 * time.Second)
			return nil
		})
		if err == nil {
			o.log("%s took the date", who)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	o.log("%s never got in", who)
}

// park fills every connection of a pool with ONE batch of legal long queries —
// the shape that reproduced E4. pg_sleep(d) is inside the 30s statement_timeout
// the work pool runs with, so nothing here is a fault.
func park(ctx context.Context, o *occupancy, db *database.DB, n int, delay, d time.Duration) {
	for i := 0; i < n; i++ {
		go func() {
			time.Sleep(delay)
			_, _ = db.Pool.Exec(ctx, fmt.Sprintf("SELECT pg_sleep(%f)", d.Seconds()))
		}()
	}
	go func() {
		time.Sleep(delay)
		o.log("work pool SATURATED: %d/%d connections parked in a legal %s query", n, n, d)
	}()
}

// oversubscribe is the other ordinary shape: MORE concurrent legal work than the
// pool has connections, so a newcomer waits for the backlog. It is what
// reproduced E5.
func oversubscribe(ctx context.Context, o *occupancy, db *database.DB, n int, delay, each time.Duration) {
	go func() {
		time.Sleep(delay)
		o.log("work pool OVERSUBSCRIBED: %d concurrent legal %s queries queued for its connections", n, each)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = db.Pool.Exec(ctx, fmt.Sprintf("SELECT pg_sleep(%f)", each.Seconds()))
			}()
		}
		wg.Wait()
		o.log("work pool backlog drained")
	}()
}

// TestPostgresE4WorkPoolPressureCannotTakeALiveHoldersDate is E4, re-run.
func TestPostgresE4WorkPoolPressureCannotTakeALiveHoldersDate(t *testing.T) {
	work, holds := pgTest(t, 5)
	h := pgHold(holds)
	date := testDate()
	o := newOccupancy()
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := holder(ctx, o, h, work, date, "A", 45*time.Second); err != nil {
			o.log("A's hold returned: %v", err)
			t.Errorf("A's hold ended with %v — under pressure that is entirely legal, a live holder must keep its date", err)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	park(ctx, o, work, 5, 1800*time.Millisecond, 28*time.Second)
	go func() {
		defer wg.Done()
		contender(ctx, o, h, date, "B", 40*time.Second)
	}()
	wg.Wait()

	if peak := o.report(t); peak > 1 {
		t.Errorf("%d writers held one dispatch date at once — E4 still reproduces", peak)
	}
}

// TestPostgresE5OneInstancesPressureCannotTakeAnothersDate is E5, re-run.
func TestPostgresE5OneInstancesPressureCannotTakeAnothersDate(t *testing.T) {
	work1, holds1 := pgTest(t, 5)
	_, holds2 := pgTest(t, 5) // instance-2 is perfectly healthy and does nothing
	h1, h2 := pgHold(holds1), pgHold(holds2)
	date := testDate()
	o := newOccupancy()
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := holder(ctx, o, h1, work1, date, "instance-1", 45*time.Second); err != nil {
			o.log("instance-1's hold returned: %v", err)
			t.Errorf("instance-1's hold ended with %v — nothing failed except its own pool being busy", err)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	oversubscribe(ctx, o, work1, 60, 1800*time.Millisecond, 3*time.Second)
	go func() {
		defer wg.Done()
		contender(ctx, o, h2, date, "instance-2", 40*time.Second)
	}()
	wg.Wait()

	if peak := o.report(t); peak > 1 {
		t.Errorf("%d writers held one dispatch date at once — E5 still reproduces", peak)
	}
}

// ---------------------------------------------------------------------------
// the substrate itself
// ---------------------------------------------------------------------------

// TestPostgresTheHoldIsExclusiveAndReleasable is the SQL, asserted rather than
// asserted-about. datehold_test.go's double cannot say any of this.
func TestPostgresTheHoldIsExclusiveAndReleasable(t *testing.T) {
	_, holds := pgTest(t, 2)
	s := pgDateHolds{pool: holds.Pool}
	ctx := context.Background()
	date := testDate()

	first, err := s.acquire(ctx, date)
	if err != nil || first == nil {
		t.Fatalf("acquire: %v", err)
	}
	second, err := s.acquire(ctx, date)
	if err != nil {
		t.Fatalf("a contended acquire must report a busy date, not an error: %v", err)
	}
	if second != nil {
		t.Fatal("one date was handed to two holders")
	}
	if other, err := s.acquire(ctx, "2027-01-01"); err != nil || other == nil {
		t.Fatalf("a different date must not contend: %v", err)
	} else {
		other.release(ctx)
	}
	if ours, err := first.stillOurs(ctx); err != nil || !ours {
		t.Fatalf("the live holder cannot confirm its own hold: ours=%v err=%v", ours, err)
	}
	first.release(ctx)

	third, err := s.acquire(ctx, date)
	if err != nil || third == nil {
		t.Fatalf("a released date must be immediately reclaimable: %v", err)
	}
	third.release(ctx)
}

// TestPostgresAReleasedHoldLeavesNoLockOnItsConnection is the leak that would be
// invisible for exactly as long as it took to exhaust the hold pool.
//
// Advisory locks are COUNTED per session. A connection handed back to the pool
// still holding one would be taken re-entrantly by the next hold, counted twice,
// and released once — shutting that dispatch date for the life of the process,
// with nothing in the application able to see it.
func TestPostgresAReleasedHoldLeavesNoLockOnItsConnection(t *testing.T) {
	_, holds := pgTest(t, 2)
	admin := adminConn(t)
	s := pgDateHolds{pool: holds.Pool}
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		date := fmt.Sprintf("2028-01-%02d", i%28+1)
		h, err := s.acquire(ctx, date)
		if err != nil || h == nil {
			t.Fatalf("acquire %s: %v", date, err)
		}
		h.release(ctx)
	}
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND classid = $1::oid`, dateHoldClass).Scan(&n); err != nil {
		t.Fatalf("count advisory locks: %v", err)
	}
	if n != 0 {
		t.Errorf("%d dispatch-date advisory lock(s) survive their holds — a pooled connection is carrying one, and the date it names can never be taken again", n)
	}
}

// TestPostgresAHolderWhoseSessionDiesLosesTheDateAndIsToldSo is the property
// that replaces the lease's TTL, and the reason the watcher still exists.
//
// A session lock is released when its session ends — that is what makes a
// crashed holder harmless without a clock. The other half is that the holder
// must NOTICE: if its session went away while its work is still running, another
// writer is now entitled to the date and this one has to stop.
func TestPostgresAHolderWhoseSessionDiesLosesTheDateAndIsToldSo(t *testing.T) {
	_, holds := pgTest(t, 2)
	admin := adminConn(t)
	h := pgHold(holds)
	h.checkEvery = 300 * time.Millisecond
	date := testDate()
	ctx := context.Background()

	stopped := make(chan struct{})
	err := h.run(ctx, date, func(work context.Context) error {
		// Kill the session holding the lock, from outside — the shape of a
		// Postgres failover, an admin's pg_terminate_backend, or a machine
		// that went away.
		if _, err := admin.Exec(ctx, `
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			 WHERE application_name = 'ailm-date-holds' AND pid <> pg_backend_pid()`); err != nil {
			return fmt.Errorf("terminate the hold session: %w", err)
		}
		select {
		case <-work.Done():
			close(stopped)
			return nil
		case <-time.After(10 * time.Second):
			return fmt.Errorf("the work ran on after its hold's session was terminated")
		}
	})
	select {
	case <-stopped:
	default:
		t.Fatalf("the work was never stopped: %v", err)
	}
	if err == nil {
		t.Fatal("losing the hold mid-flight must be reported")
	}
}

// TestTheHoldPoolIsSeparateFromTheWorkPool states the wiring where the pools are
// real, so "two pools" is not merely two nil pointers being unequal.
func TestTheHoldPoolIsSeparateFromTheWorkPool(t *testing.T) {
	work, holds := pgTest(t, 2)
	r := NewRepository(work, holds)
	pg, ok := r.hold.holds.(pgDateHolds)
	if !ok {
		t.Fatalf("the production hold is a %T", r.hold.holds)
	}
	if pg.pool == work.Pool {
		t.Fatal("the hold draws on the WORK pool — this is the coupling that let ordinary pool pressure hand one dispatch date to two writers")
	}
	if pg.pool != holds.Pool {
		t.Fatal("the hold does not draw on the hold pool")
	}
	var name string
	c, err := holds.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	if err := c.QueryRow(context.Background(), `SHOW application_name`).Scan(&name); err != nil {
		t.Fatalf("show application_name: %v", err)
	}
	if name != "ailm-date-holds" {
		t.Errorf("the hold connections call themselves %q — an operator asking pg_stat_activity what is holding a date needs them to say so", name)
	}
}

// ---------------------------------------------------------------------------
// Acceptance II — the availability table
// ---------------------------------------------------------------------------

// availResult is one row of the table.
type availResult struct {
	n            int
	probes, fail int
	refused      int
	peakIdleTx   int
	wall         time.Duration
}

func (r availResult) String() string {
	return fmt.Sprintf("N=%-3d  probes %2d, FAILED %2d   peak idle-in-transaction %d   operations refused %d   wall %.1fs",
		r.n, r.probes, r.fail, r.peakIdleTx, r.refused, r.wall.Seconds())
}

// measureAvailability runs n concurrent slow operations, each holding its OWN
// date — which the claim deliberately does not serialize against each other —
// while probing what /healthz/ready probes (db.Pool.Ping) on a 2s deadline every
// 500ms.
func measureAvailability(t *testing.T, db *database.DB, admin *pgx.Conn, n int, hold func(ctx context.Context, date string, fn func(context.Context) error) error) availResult {
	t.Helper()
	ctx := context.Background()
	start := time.Now()

	var refused atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			date := fmt.Sprintf("2029-%02d-%02d", i/28+1, i%28+1)
			err := hold(ctx, date, func(context.Context) error {
				// The ERP round-trips. No database work happens here; this is
				// exactly the window the transaction-scoped shape held a pooled
				// connection across.
				time.Sleep(3 * time.Second)
				return nil
			})
			if err != nil {
				refused.Add(1)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	// The probes fire on their OWN goroutines: a load balancer does not skip a
	// health check because the previous one is still hanging, and counting them
	// on the ticker's thread would let one failing probe hide the next two
	// behind its own 2s deadline.
	var probes, fails atomic.Int64
	var peak int
	var probeWG sync.WaitGroup
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	sample := time.NewTicker(200 * time.Millisecond)
	defer sample.Stop()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-sample.C:
			var c int
			if err := admin.QueryRow(ctx,
				`SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction' AND pid <> pg_backend_pid()`).Scan(&c); err == nil && c > peak {
				peak = c
			}
		case <-tick.C:
			probeWG.Add(1)
			go func() {
				defer probeWG.Done()
				pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err := db.Pool.Ping(pctx)
				cancel()
				probes.Add(1)
				if err != nil {
					fails.Add(1)
				}
			}()
		}
	}
	probeWG.Wait()
	return availResult{n: n, probes: int(probes.Load()), fail: int(fails.Load()), refused: int(refused.Load()), peakIdleTx: peak, wall: time.Since(start)}
}

// oldXactHold is the shape the advisory-lock commit shipped and the lease
// replaced: a TRANSACTION-scoped advisory lock, so the work runs inside a
// transaction and pins one pooled connection for its whole duration. It is
// reconstructed here only as the column the table is read against.
func oldXactHold(db *database.DB) func(ctx context.Context, date string, fn func(context.Context) error) error {
	return func(ctx context.Context, date string, fn func(context.Context) error) error {
		return db.RunInTx(ctx, func(ctx context.Context) error {
			var got bool
			if err := db.GetExecutor(ctx).QueryRow(ctx,
				`SELECT pg_try_advisory_xact_lock($1, hashtext($2))`, dateHoldClass, date).Scan(&got); err != nil {
				return err
			}
			if !got {
				return ErrDateBusy
			}
			return fn(ctx)
		})
	}
}

// TestPostgresAvailabilityUnderConcurrentHolds is acceptance II: N concurrent
// slow operations on DIFFERENT dates must not fail an unrelated health probe.
//
// It is a measurement AND an assertion. The measurement is the table, which is
// the only way to compare this against what it replaced; the assertion is that
// the session-lock column stays at zero, because a table nobody fails on is a
// table that stops being read.
func TestPostgresAvailabilityUnderConcurrentHolds(t *testing.T) {
	// The hold pool is sized ABOVE the largest N on purpose. The row has to
	// measure 25 operations actually holding 25 dates; a smaller hold pool would
	// turn most of them into instant refusals and the zero below would be the
	// zero of work that never happened. refused is reported and asserted for
	// exactly that reason.
	work, holds := pgTestWithHolds(t, 5, 32)
	admin := adminConn(t)
	h := pgHold(holds)

	t.Log("OLD — advisory lock inside the transaction, on the WORK pool:")
	for _, n := range []int{1, 5, 25} {
		t.Log("  " + measureAvailability(t, work, admin, n, oldXactHold(work)).String())
	}
	t.Log("NEW — session lock on the dedicated hold pool:")
	for _, n := range []int{1, 5, 25} {
		r := measureAvailability(t, work, admin, n, h.run)
		t.Log("  " + r.String())
		if r.fail != 0 {
			t.Errorf("N=%d: %d of %d health probes failed while %d slow operations held %d DIFFERENT dates", n, r.fail, r.probes, n, n)
		}
		if r.peakIdleTx != 0 {
			t.Errorf("N=%d: peak %d connections idle in transaction — the hold is wrapping the work in a transaction again", n, r.peakIdleTx)
		}
		if r.refused != 0 {
			t.Errorf("N=%d: %d of the %d operations never held their date, so this row measures refusals rather than availability under load", n, r.refused, n)
		}
	}
}

// TestPostgresAnExhaustedHoldPoolRefusesFastAndTruthfully.
//
// The dedicated pool is the price of the substrate. Running out of it must be a
// prompt, honest refusal — not a queued request, and not a claim that the date
// the caller asked for is busy, which would be false.
func TestPostgresAnExhaustedHoldPoolRefusesFastAndTruthfully(t *testing.T) {
	_, holds := pgTest(t, 2)
	s := pgDateHolds{pool: holds.Pool, wait: 300 * time.Millisecond}
	ctx := context.Background()

	var open []heldDate
	defer func() {
		for _, h := range open {
			h.release(ctx)
		}
	}()
	for i := 0; i < testHoldConns; i++ {
		h, err := s.acquire(ctx, fmt.Sprintf("2030-03-%02d", i+1))
		if err != nil || h == nil {
			t.Fatalf("hold %d of %d: %v", i+1, testHoldConns, err)
		}
		open = append(open, h)
	}

	start := time.Now()
	h, err := s.acquire(ctx, "2030-04-01")
	elapsed := time.Since(start)
	if h != nil {
		open = append(open, h)
		t.Fatal("a pool of 4 handed out a fifth hold")
	}
	if err == nil {
		t.Fatal("an exhausted hold pool answered 'this date is busy' about a date nobody is holding")
	}
	if !errors.Is(err, ErrDateHoldsFull) {
		t.Errorf("want ErrDateHoldsFull, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the refusal took %s — contention must cost a fast refusal, never a parked request", elapsed)
	}
}

// TestPostgresTheOwnershipCheckIsAboutThisBackendNotTheDatabase.
//
// stillOurs asks whether THIS BACKEND holds the lock, not whether the lock
// exists. The difference is the whole of the deployment's safety in front of a
// transaction-mode connection pooler, which hands each statement to whichever
// server connection is free: the lock would be taken on one session, stranded
// there for ever, and never held by the session doing the work. Dropping the
// `pid = pg_backend_pid()` clause makes the check answer "yes, somebody holds
// it" — which is true, useless, and exactly the answer that lets two writers
// through.
//
// Asserted with two real sessions, which is as close to that pooler as this
// harness can get without one.
func TestPostgresTheOwnershipCheckIsAboutThisBackendNotTheDatabase(t *testing.T) {
	_, holds := pgTest(t, 2)
	other := adminConn(t)
	ctx := context.Background()
	date := testDate()

	s := pgDateHolds{pool: holds.Pool}
	h, err := s.acquire(ctx, date)
	if err != nil || h == nil {
		t.Fatalf("acquire: %v", err)
	}
	defer h.release(ctx)

	// A session that does NOT hold the lock asks the same question.
	var ours bool
	if err := other.QueryRow(ctx, sqlHoldIsOurs, dateHoldClass, date).Scan(&ours); err != nil {
		t.Fatalf("ask from another session: %v", err)
	}
	if ours {
		t.Error("a session that does not hold the lock was told it does — the ownership check is about the lock's existence rather than about this backend, and every statement issued through a transaction-pooling proxy would pass it")
	}
	// And the holder still says yes, so the check is not simply always false.
	if mine, err := h.stillOurs(ctx); err != nil || !mine {
		t.Fatalf("the holder cannot confirm its own hold: ours=%v err=%v", mine, err)
	}
}
