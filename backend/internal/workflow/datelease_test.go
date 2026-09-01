// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// The date hold is the one piece of this serialization whose mistakes are
// SILENT. A hold that is never released shuts a dispatch date until a restart;
// a hold that releases somebody else's lease lets two writers onto one date and
// every acceptance trial in this package goes on passing, because the fake
// store in those trials is not this code. So the orchestration — fail fast,
// renew while working, stop when the lease is lost, release exactly once and
// only your own — is tested here against a lease table in memory.
//
// What is NOT tested here is the SQL: the atomicity of the acquire upsert is a
// property of Postgres and of that one statement, and asserting it needs a
// database this suite does not have. It is stated in datelease.go and in
// migration 005 instead, and it is the one thing in this file's subject matter
// that a reader has to take on the code's word.

// memLeases is a lease table in memory with the same contract as the Postgres
// one: acquire succeeds only on a free or EXPIRED date, and renew/release are
// conditioned on the holder token.
type memLeases struct {
	mu   sync.Mutex
	now  func() time.Time
	held map[string]memLease

	acquires, renews, releases int
	acquireErr, renewErr       error
	// stolen, when set, makes the next renew report the lease as somebody
	// else's without changing the table — the crashed-and-taken-over case.
	stolen bool
}

type memLease struct {
	holder  string
	expires time.Time
}

func newMemLeases() *memLeases {
	return &memLeases{now: time.Now, held: map[string]memLease{}}
}

func (m *memLeases) acquire(_ context.Context, date, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquires++
	if m.acquireErr != nil {
		return false, m.acquireErr
	}
	if cur, ok := m.held[date]; ok && cur.expires.After(m.now()) {
		return false, nil
	}
	m.held[date] = memLease{holder: holder, expires: m.now().Add(ttl)}
	return true, nil
}

func (m *memLeases) renew(_ context.Context, date, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renews++
	if m.renewErr != nil {
		return false, m.renewErr
	}
	if m.stolen {
		return false, nil
	}
	cur, ok := m.held[date]
	if !ok || cur.holder != holder {
		return false, nil
	}
	m.held[date] = memLease{holder: holder, expires: m.now().Add(ttl)}
	return true, nil
}

func (m *memLeases) release(_ context.Context, date, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releases++
	if cur, ok := m.held[date]; ok && cur.holder == holder {
		delete(m.held, date)
	}
	return nil
}

func (m *memLeases) heldBy(date string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.held[date]
	return l.holder, ok
}

func (m *memLeases) counts() (int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acquires, m.renews, m.releases
}

// testHold is a dateHold with everything scaled to milliseconds so the timing
// properties can be asserted rather than described.
func testHold(l dateLeaseStore) dateHold {
	return dateHold{leases: l, ttl: 60 * time.Millisecond, renewEvery: 15 * time.Millisecond, ceiling: 300 * time.Millisecond, releaseWait: time.Second}
}

// TestASecondHolderOfTheSameDateIsRefusedHavingRunNothing is the exclusion, and
// the fail-fast decision underneath it.
func TestASecondHolderOfTheSameDateIsRefusedHavingRunNothing(t *testing.T) {
	l := newMemLeases()
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
		t.Fatalf("the first holder should get the date: %v", err)
	}
	if inner != 0 {
		t.Errorf("the refused holder ran its work %d time(s) — a refusal that has already acted is not a refusal", inner)
	}
	if holder, ok := l.heldBy("2026-06-26"); ok {
		t.Errorf("the date is still held by %q after both calls returned — one refusal has shut the day", holder)
	}
}

// TestAdjacentDatesDoNotContend pins the resource the hold names. A hold
// accidentally taken per-INSTALL rather than per-date would pass every other
// test here and quietly serialize the dealer's whole week.
func TestAdjacentDatesDoNotContend(t *testing.T) {
	l := newMemLeases()
	h := testHold(l)
	err := h.run(context.Background(), "2026-06-26", func(context.Context) error {
		return h.run(context.Background(), "2026-06-27", func(context.Context) error { return nil })
	})
	if err != nil {
		t.Fatalf("two different dates must be holdable at once, got %v", err)
	}
}

// TestTheDateIsReleasedWhateverTheWorkReturns guards the release path.
//
// A push refuses on a dozen gates and errors from several more. If any of them
// left the date held, the first refused push of the morning would shut the day
// for every dispatcher until the lease expired — and with a panic, the release
// has to happen without the work's cooperation at all.
func TestTheDateIsReleasedWhateverTheWorkReturns(t *testing.T) {
	boom := errors.New("gable is down")
	t.Run("work returns an error", func(t *testing.T) {
		l := newMemLeases()
		if err := testHold(l).run(context.Background(), "2026-06-26", func(context.Context) error {
			return boom
		}); !errors.Is(err, boom) {
			t.Fatalf("the work's own error must reach the caller, got %v", err)
		}
		if _, ok := l.heldBy("2026-06-26"); ok {
			t.Fatal("a failed piece of work left the date held")
		}
	})
	t.Run("the date is immediately reusable", func(t *testing.T) {
		l := newMemLeases()
		h := testHold(l)
		for i := 0; i < 3; i++ {
			if err := h.run(context.Background(), "2026-06-26", func(context.Context) error { return nil }); err != nil {
				t.Fatalf("attempt %d: %v", i, err)
			}
		}
		if a, _, r := l.counts(); a != 3 || r != 3 {
			t.Fatalf("3 holds took %d and released %d — release must be exactly once per hold", a, r)
		}
	})
}

// TestEachHoldTakesItsOwnHolderToken is the fencing rule.
//
// holder is per-ACQUISITION, not per-process, and both renew and release are
// conditioned on it. That is what stops a holder whose lease EXPIRED and was
// taken over by somebody else from extending or releasing the NEW holder's
// lease on its way out — which would hand a third writer the date while the
// second was still working: the two-writer race with an extra step.
//
// A process-wide (or constant) token would pass every other test in this file
// and fail only under the exact interleaving it exists to prevent.
func TestEachHoldTakesItsOwnHolderToken(t *testing.T) {
	l := newTokenRecordingLeases()
	h := testHold(l)
	for i := 0; i < 3; i++ {
		if err := h.run(context.Background(), "2026-06-26", func(context.Context) error { return nil }); err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
	}
	if len(l.tokens) != 3 {
		t.Fatalf("saw %d acquisitions, want 3", len(l.tokens))
	}
	seen := map[string]bool{}
	for _, tok := range l.tokens {
		if tok == "" {
			t.Fatal("a hold was taken with an empty holder token — release and renewal can then match anybody's lease")
		}
		if seen[tok] {
			t.Fatalf("holder token %q was reused across acquisitions — a stale holder can release the lease that replaced it", tok)
		}
		seen[tok] = true
	}
	// Release must present the SAME token the acquisition used, or the
	// condition protects nothing.
	for i, tok := range l.tokens {
		if l.released[i] != tok {
			t.Errorf("hold %d acquired as %q and released as %q", i, tok, l.released[i])
		}
	}
}

// tokenRecordingLeases records the holder token of every acquisition and
// release, which is the only way to see a token this code never exposes.
type tokenRecordingLeases struct {
	*memLeases
	mu       sync.Mutex
	tokens   []string
	released []string
}

func newTokenRecordingLeases() *tokenRecordingLeases {
	return &tokenRecordingLeases{memLeases: newMemLeases()}
}

func (l *tokenRecordingLeases) acquire(ctx context.Context, date, holder string, ttl time.Duration) (bool, error) {
	ok, err := l.memLeases.acquire(ctx, date, holder, ttl)
	if ok {
		l.mu.Lock()
		l.tokens = append(l.tokens, holder)
		l.mu.Unlock()
	}
	return ok, err
}

func (l *tokenRecordingLeases) release(ctx context.Context, date, holder string) error {
	l.mu.Lock()
	l.released = append(l.released, holder)
	l.mu.Unlock()
	return l.memLeases.release(ctx, date, holder)
}

// TestAnExpiredHoldIsReclaimable is what replaces the advisory lock's one
// genuinely nice property: a process that DIES holding a date must not shut
// that date for ever.
//
// The lease it left behind is not released by anything, so the next writer has
// to be able to take it over once it has expired. Without this, the whole
// design trades a race for an outage.
func TestAnExpiredHoldIsReclaimable(t *testing.T) {
	l := newMemLeases()
	// A holder that crashed: its lease is in the table, and expired.
	l.held["2026-06-26"] = memLease{holder: "a-process-that-died", expires: time.Now().Add(-time.Second)}

	ran := false
	if err := testHold(l).run(context.Background(), "2026-06-26", func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("an expired hold must be reclaimable, got %v", err)
	}
	if !ran {
		t.Fatal("the reclaiming writer never ran")
	}
}

// TestALongHoldIsRenewedRatherThanLost is why the TTL can be short.
//
// The TTL is what bounds a crashed holder's outage, so it wants to be seconds,
// not minutes. But a real push legitimately runs longer than that — up to one
// 15s ERP timeout per truck. Both are only possible if a LIVE holder keeps
// extending its lease, which is what this asserts: work lasting several TTLs
// finishes holding the same lease it started with.
func TestALongHoldIsRenewedRatherThanLost(t *testing.T) {
	l := newMemLeases()
	h := testHold(l)

	var sawCancel bool
	err := h.run(context.Background(), "2026-06-26", func(ctx context.Context) error {
		// Four TTLs of work. Without renewal the lease would have expired
		// three times over.
		select {
		case <-time.After(4 * h.ttl):
		case <-ctx.Done():
			sawCancel = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a renewed hold must not fail, got %v", err)
	}
	if sawCancel {
		t.Fatal("the work was cancelled part-way — a live holder lost a date it was still using")
	}
	if _, renews, _ := l.counts(); renews == 0 {
		t.Fatal("the hold was never renewed, so it survived only because this test was fast enough")
	}
}

// TestLosingTheLeaseStopsTheWork is the rule that keeps the expiry honest.
//
// expires_at is a clock, not a fact. If a holder can be declared expired while
// it is still running, then it MUST stop when it notices — because from that
// instant somebody else is entitled to write this date, and carrying on is
// precisely the two-writer race the hold exists to prevent.
func TestLosingTheLeaseStopsTheWork(t *testing.T) {
	l := newMemLeases()
	l.stolen = true // every renewal will report the lease as no longer ours
	h := testHold(l)
	// The ceiling is pushed far out on purpose. With the test's ordinary
	// ceiling the work is cut off either way, so the cancel-on-loss could be
	// deleted and this test would still pass — it would be measuring the
	// ceiling. Here the ONLY thing that can stop the work is noticing the
	// lease is gone.
	h.ceiling = time.Minute

	stopped := false
	err := h.run(context.Background(), "2026-06-26", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			stopped = true
		case <-time.After(2 * time.Second):
		}
		return nil
	})
	if !stopped {
		t.Fatal("the work ran on after the lease stopped being ours")
	}
	if !errors.Is(err, ErrDateHoldLost) {
		t.Fatalf("a hold lost mid-flight must be reported as such, got %v", err)
	}
}

// TestTheWorksOwnAccountSurvivesALostHold is the other half of that rule.
//
// Losing the hold cancels the work, so the work usually returns its own
// description of where it got to — "push stopped at truck 3, two trucks are now
// live on the dispatch board". That sentence is what the dispatcher has to act
// on, and replacing it with "the hold was lost" would trade the state of the
// dealer's board for a description of this module's plumbing.
func TestTheWorksOwnAccountSurvivesALostHold(t *testing.T) {
	l := newMemLeases()
	l.stolen = true
	partial := refusedf("push stopped at truck 3: 2 of 5 truck(s) are now live on the dispatch board")

	h := testHold(l)
	h.ceiling = time.Minute // as above: the loss, not the ceiling, must be what stops the work
	err := h.run(context.Background(), "2026-06-26", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
		return partial
	})
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the work's own refusal must reach the caller, got %v", err)
	}
	if refusal.Msg != partial.(*Refusal).Msg {
		t.Fatalf("the refusal was rewritten: %q", refusal.Msg)
	}
}

// TestAHoldHasACeilingEvenWhileItIsBeingRenewed is the answer to "and if the
// process is alive but wedged?".
//
// Renewal keeps a WORKING holder's date. It would also keep a stuck one's — an
// ERP that accepts the connection and never replies renews just as happily as
// one that is answering. The ceiling is what stops that being an unbounded hold
// on a dispatcher's day, and it is deliberately independent of the caller's own
// context: a client willing to wait for ever must not be able to shut a date
// for ever.
func TestAHoldHasACeilingEvenWhileItIsBeingRenewed(t *testing.T) {
	l := newMemLeases()
	h := testHold(l)

	started := time.Now()
	stopped := false
	// The caller's context has no deadline at all.
	err := h.run(context.Background(), "2026-06-26", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			stopped = true
		case <-time.After(30 * time.Second):
		}
		return ctx.Err()
	})
	if !stopped {
		t.Fatal("the work was never cut off — a wedged holder can shut a dispatch date indefinitely")
	}
	if elapsed := time.Since(started); elapsed > 5*h.ceiling {
		t.Fatalf("the hold lasted %s, well past its %s ceiling", elapsed, h.ceiling)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the cut-off work must report a deadline, got %v", err)
	}
	if _, ok := l.heldBy("2026-06-26"); ok {
		t.Fatal("the date is still held after the ceiling fired")
	}
}

// TestProductionHoldTimingsAreCoherent pins the three constants against each
// other rather than against a number.
//
// Their absolute values are a judgement call. Their ORDER is not: a renewal
// interval at or above the TTL renews a lease that has already expired, and a
// ceiling at or below the TTL means the hold is cut off before its first
// renewal ever matters. Either mistake is invisible until a date is lost or a
// push is killed mid-flight in production.
func TestProductionHoldTimingsAreCoherent(t *testing.T) {
	if dateLeaseRenew*2 > dateLeaseTTL {
		t.Errorf("renewal every %s against a %s lease leaves no margin for a slow renewal — a live holder can lose a date it is still using", dateLeaseRenew, dateLeaseTTL)
	}
	if dateHoldCeiling <= dateLeaseTTL {
		t.Errorf("a %s ceiling under a %s lease makes renewal pointless", dateHoldCeiling, dateLeaseTTL)
	}
	if dateHoldCeiling > 15*time.Minute {
		t.Errorf("a %s ceiling is not a bound a dispatcher can wait out", dateHoldCeiling)
	}
	if dateLeaseRelease <= 0 {
		t.Error("release needs its own timeout: it runs after the work's context is already done")
	}
}

// TestAnUnavailableLeaseTableRefusesRatherThanProceeds is the failure direction.
//
// If the lease cannot be taken, the ONLY safe answer is to do nothing: running
// the work anyway would put an unserialized writer on the date, which is the
// defect. The error is also not ErrDateBusy — "the database is unreachable" and
// "somebody else is pushing" need different answers from an operator.
func TestAnUnavailableLeaseTableRefusesRatherThanProceeds(t *testing.T) {
	l := newMemLeases()
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
		t.Fatal("an unavailable lease table must be reported")
	}
	if errors.Is(err, ErrDateBusy) {
		t.Fatalf("an unreachable database must not be reported as a busy date: %v", err)
	}
}

// TestConcurrentHoldersOfOneDateNeverOverlap is the property itself, asserted
// on the orchestration rather than through the HTTP surface: many goroutines,
// one date, and never two inside at once.
func TestConcurrentHoldersOfOneDateNeverOverlap(t *testing.T) {
	l := newMemLeases()
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

// TestTheDateHoldDoesNotRunItsWorkInsideATransaction is acceptance IV stated
// against the real Repository rather than against the in-memory double.
//
// This is the property that regressed and that no test could see. The first
// version of this serialization took pg_try_advisory_xact_lock, which is
// released by COMMIT — so holding a date meant holding an open transaction, and
// an open transaction means one of MaxConns=25 pooled connections pinned across
// every GableLBM round-trip inside the hold, up to one 15s ERP timeout per
// truck. Since the hold deliberately does NOT serialize different dates against
// each other, twenty-five slow pushes on twenty-five different days could take
// every connection in the pool and stall every endpoint in the service.
//
// The assertion is exact: whatever executor the work sees must be the POOL, not
// a transaction. A future edit that reintroduces RunInTx around the hold fails
// here and nowhere else.
func TestTheDateHoldDoesNotRunItsWorkInsideATransaction(t *testing.T) {
	db := &database.DB{} // deliberately pool-less: opening a transaction would need one
	r := &Repository{db: db, hold: testHold(newMemLeases())}

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
