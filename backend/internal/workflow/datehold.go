// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The exclusive hold on one dispatch DATE.
//
// # Three substrates, and why this is the third
//
// The hold has been three things, and the argument between them is the whole
// content of this file.
//
//  1. pg_try_advisory_xact_lock — a TRANSACTION-scoped advisory lock, taken as
//     the first statement of a transaction that wrapped the entire operation.
//     Correct and unstarvable: the holder cannot be evicted from its own lock,
//     because it IS the connection. Unsurvivable in cost: holding a date meant
//     holding one of MaxConns=25 pooled connections across every GableLBM
//     round-trip, up to one 15s ERP timeout per truck, and the hold deliberately
//     does not serialize DIFFERENT dates against each other — so twenty-five
//     slow pushes on twenty-five days took the pool and stalled every endpoint
//     in the service, /health included. Measured: 2 of 6 readiness probes failed
//     at 5 concurrent slow operations, 26 of 30 at 25, peak "idle in
//     transaction" 5.
//
//  2. A LEASE ROW with a TTL and a renewal loop. The connection went back to the
//     pool for the whole of the work, which fixed the availability cost
//     completely (0 probes failed at every level, peak "idle in transaction" 0).
//     It broke mutual exclusion. The lease survives only if the holder wins a
//     POOLED connection every renewal period, and the renewal was a synchronous
//     un-timed pooled query — so ordinary pressure on the work pool, which is
//     the very harm this work exists to fix, silently took a date away from a
//     live holder. Reproduced against PostgreSQL 16 with the shipped constants:
//     one pool, all five connections parked for 28s in queries well inside the
//     30s statement_timeout, and a second writer ACQUIRED the same date at
//     t=30.03s while the first was still writing it. Two instances, two pools,
//     Postgres idle, no crash and no partition: only instance-1's pool
//     oversubscribed by ordinary legal traffic, and instance-2 held the same
//     date from t=30.06s to t=32.06s while instance-1 went on writing it until
//     t=38.03s. Peak concurrent holders of one date: 2.
//
//  3. What is here: pg_try_advisory_lock — a SESSION-scoped advisory lock, held
//     on a connection from a SMALL DEDICATED POOL that the work never touches.
//
// The third gets both properties, and it gets them for one reason: the hold and
// the work no longer compete for the same resource. The holder cannot be
// starved out of its own lock because it is not asking for anything — it
// already has the connection, checked out, for the duration. And the work pool
// is untouched, because nothing wraps the work in a transaction and no pooled
// connection is pinned across an ERP round-trip. There is no TTL, no renewal, no
// clock and no sweeper: a lock is a fact about a session, not a bet about time.
//
// # What a session lock costs, and what pays for it
//
// The reason (1) chose transaction scope was that a session lock "needs an
// explicit unlock or the session to end", so a killed process could wedge a
// date. Both halves of that are answered here rather than argued away:
//
//   - A killed process drops its TCP connections, the server sees EOF, the
//     backend exits, and every advisory lock it held goes with it. This is the
//     ordinary case (SIGKILL, panic, container eviction) and it needs nothing.
//   - A machine that vanishes without closing its sockets — a power cut, a
//     severed network — leaves the backend waiting on a socket that will never
//     speak again. That is why the hold pool asks the SERVER for TCP keepalives
//     (see dateHoldPoolConfig): Postgres reaps the session in about a minute
//     instead of whenever the OS default expires. An operator who cannot wait
//     that long has pg_terminate_backend, and the hold connections name
//     themselves in pg_stat_activity so they can be found.
//
// The genuinely new cost is one connection per CONCURRENTLY HELD DATE, from a
// pool sized for that and nothing else. Exhausting it refuses a writer that
// could otherwise have proceeded — it is a real refusal and it is reported as
// what it is, not disguised as a busy date.
//
// # What it still does not do
//
// Nothing here makes the ERP write and the ledger write atomic. A process that
// dies between PushDeliveryRoute returning and the ledger being saved still
// leaves a route on the dealer's board that no ledger names. The hold bounds who
// may write a date; it does not reconcile an orphan. That is the reconciler's
// job and it is not done here.

// dateHoldStore is the substrate the hold is made of. It is an interface so the
// orchestration above it — fail fast, watch, stop when the hold is gone, release
// exactly once — can be tested without a Postgres, which is the only part of
// this file where a mistake is silent.
type dateHoldStore interface {
	// acquire takes date exclusively. Exactly one of three things is true of
	// the result, and callers depend on being able to tell them apart:
	//
	//	(hold, nil)  the date is ours until hold.release
	//	(nil,  nil)  somebody else holds it — refuse, having done nothing
	//	(nil,  err)  we do not know — refuse, having done nothing
	//
	// The middle case is the ONLY one that may be reported as a busy date.
	acquire(ctx context.Context, date string) (heldDate, error)
}

// heldDate is one live hold.
type heldDate interface {
	// stillOurs reports whether the hold is still ours.
	//
	// It must run on the hold's OWN resource and never on the pool the work
	// uses. That is not an optimisation: a check that has to queue behind the
	// work is a check that cannot answer at the moment it matters, which is
	// exactly how substrate (2) lost a date to its own work.
	//
	//	(true,  nil)  verified ours
	//	(false, nil)  verified NOT ours — the work must stop
	//	(false, err)  could not tell — try again next tick
	stillOurs(ctx context.Context) (bool, error)
	// release drops the hold. It reports nothing because the caller has
	// nothing to do about a failure: the implementation's obligation is that a
	// hold it could not cleanly release is DESTROYED rather than left half-held.
	release(ctx context.Context)
}

// Production timings.
//
// There is no TTL and no renewal period here, because the hold is not a bet
// about time. checkEvery is how quickly a hold that has been lost some other
// way — the session terminated, the connection dropped, a transaction-mode
// pooler handing us a different backend — stops the work that no longer owns
// the date. ceiling is the answer to "and if the process is alive but the ERP
// never replies?": long enough for a real multi-truck push with slow
// round-trips, short enough that a dispatcher is not locked out of their day.
const (
	dateHoldCheck   = 5 * time.Second
	dateHoldCeiling = 4 * time.Minute
	// dateHoldWait bounds BOTH the wait for a hold connection and the two tiny
	// statements taken on it. A hold that cannot be taken quickly must be
	// refused quickly: the whole design of this serialization is that
	// contention costs a 409 and never a queued request.
	dateHoldWait = 2 * time.Second
	// dateHoldRelease is separate because release must still run when the
	// work's context is already done.
	dateHoldRelease = 5 * time.Second
)

// dateHold orchestrates one exclusive hold on a date.
type dateHold struct {
	holds      dateHoldStore
	checkEvery time.Duration
	// checkWait bounds ONE check. Without it a check against a socket whose
	// peer has gone silent blocks until the OS gives up, and run — which waits
	// for the watcher before releasing — would block with it. A hold must be
	// able to end even when the thing it would ask has stopped answering.
	checkWait   time.Duration
	ceiling     time.Duration
	releaseWait time.Duration
}

func newDateHold(holds dateHoldStore) dateHold {
	return dateHold{
		holds:       holds,
		checkEvery:  dateHoldCheck,
		checkWait:   dateHoldWait,
		ceiling:     dateHoldCeiling,
		releaseWait: dateHoldRelease,
	}
}

// ErrDateHoldLost is returned when a hold was taken and then lost mid-flight, so
// this caller was stopped rather than allowed to keep writing a date it no
// longer owns.
//
// It is deliberately NOT ErrDateBusy: ErrDateBusy means nothing happened, and
// this means work was interrupted part-way. Reporting the second as the first
// would tell a dispatcher "nothing was sent" about a push that may already have
// put trucks on the board.
var ErrDateHoldLost = errors.New("lost the dispatch-date hold mid-flight")

// ErrDateHoldsFull is returned when this instance is already holding as many
// dispatch dates at once as it has hold connections for.
//
// It is separate from ErrDateBusy on purpose. "Somebody else is writing 26 June"
// and "this instance is writing more days at once than it has room for" are
// both refusals that changed nothing and both retryable, but they are not the
// same fact, and the sentence a dispatcher reads must not claim the first when
// the second is true.
var ErrDateHoldsFull = errors.New("no dispatch-date hold connection is free")

// run takes the date, runs fn, and releases it.
//
// It FAILS FAST: a contended date returns ErrDateBusy having run nothing, which
// is what makes "try again" a plain repeat rather than a partial recovery.
//
// fn receives a context that is cancelled when the hold ends for any reason —
// the ceiling, a lost hold, or the caller's own context. Everything fn does
// downstream (ERP round-trips, repository writes) therefore stops with the hold
// instead of running on past it.
func (h dateHold) run(ctx context.Context, date string, fn func(ctx context.Context) error) error {
	held, err := h.holds.acquire(ctx, date)
	if err != nil {
		if errors.Is(err, ErrDateHoldsFull) {
			return err
		}
		return fmt.Errorf("take the dispatch-date hold for %s: %w", date, err)
	}
	if held == nil {
		return ErrDateBusy
	}

	// The ceiling applies whatever the caller's own deadline is, so no request
	// can hold a dispatch date longer than this even if its client waits.
	work, cancel := context.WithTimeout(ctx, h.ceiling)
	defer cancel()

	stop := make(chan struct{})
	lost := make(chan struct{})
	watching := make(chan struct{})
	go h.watch(work, date, held, stop, lost, watching, cancel)

	err = fn(work)
	close(stop)
	// The watcher and the release share ONE connection, and pgx connections are
	// not safe for concurrent use. Waiting here is what makes "exactly one user
	// at a time" a property of the code rather than of the scheduler.
	<-watching

	// Release on a context DETACHED from work: work is already cancelled by the
	// deferred cancel above on every path, and a release that cannot run
	// destroys a connection for no reason. It is still bounded.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), h.releaseWait)
	defer rcancel()
	held.release(rctx)

	// A hold lost mid-flight is reported ONLY when fn had nothing of its own to
	// say. Losing the hold cancels fn's context, so fn almost always returns its
	// own account of where it got to — "push stopped at truck 3, two trucks are
	// now live on the board" — and that account is what the dispatcher has to
	// act on. Replacing it with "the hold was lost" would trade the state of the
	// dealer's board for a description of this module's plumbing.
	select {
	case <-lost:
		if err == nil {
			return fmt.Errorf("%w: %s", ErrDateHoldLost, date)
		}
	default:
	}
	return err
}

// watch stops the work the moment the hold stops being ours.
//
// Under substrate (2) this loop was the mechanism that KEPT the hold, and being
// starved of a connection therefore lost it. Here it keeps nothing: the lock is
// held by the session, not by this goroutine, and the only thing that can take
// it away is that session ending. So a check that cannot be taken is a warning,
// and a check that comes back NO is the end of the work — the difference
// between "I could not ask" and "the answer is no" is now representable, which
// under a TTL it was not.
func (h dateHold) watch(work context.Context, date string, held heldDate, stop <-chan struct{}, lost chan<- struct{}, done chan<- struct{}, cancel context.CancelFunc) {
	defer close(done)
	t := time.NewTicker(h.checkEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-work.Done():
			return
		case <-t.C:
			cctx, ccancel := context.WithTimeout(work, h.checkWait)
			ours, err := held.stillOurs(cctx)
			ccancel()
			if err != nil {
				if work.Err() != nil {
					return
				}
				slog.Warn("could not confirm a dispatch-date hold; the lock is held by the session and this check keeps nothing, so the work continues",
					"date", date, "error", err)
				continue
			}
			if !ours {
				slog.Error("lost a dispatch-date hold while work was in flight — stopping rather than writing a date somebody else may now own",
					"date", date)
				close(lost)
				cancel()
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// dateHoldClass namespaces this module's advisory-lock keys.
//
// Postgres advisory locks live in ONE database-wide keyspace shared by every
// session and every feature that ever takes one. The two-argument form
// pg_try_advisory_lock(classid, objid) is what keeps this module's keys from
// colliding with somebody else's: the class is this constant and only the object
// id is derived from the date. Without it, any other advisory lock in this
// database that happened to hash to the same value would silently serialize
// against dispatch writes, or be serialized by them.
//
// The value is arbitrary; the only property that matters is that it is this
// module's and nobody changes it. It reads "AILM" in ASCII.
const dateHoldClass int32 = 0x41494c4d

// The object id is hashtext(date), computed by the SERVER rather than in Go.
//
// That is deliberate on two counts. The obvious one is that every instance then
// derives the same key by construction rather than by two implementations
// agreeing. The useful one is that an operator holding an incident can ask the
// same question the code asks:
//
//	SELECT pg_try_advisory_lock is irrelevant; to SEE what is held:
//	SELECT a.application_name, l.classid, l.objid
//	  FROM pg_locks l JOIN pg_stat_activity a USING (pid)
//	 WHERE l.locktype = 'advisory' AND l.classid = 1095975757;
//	-- and to test one date:
//	SELECT hashtext('2026-06-26')::oid;
//
// Two different dates hashing to the same 32-bit value would make one of them
// spuriously report as busy. That is a refusal, not a double writer — the
// collision costs availability and never safety — and at a handful of live
// dispatch dates it is not reachable in practice.
const (
	sqlTakeDateHold = `SELECT pg_try_advisory_lock($1, hashtext($2))`
	sqlDropDateHold = `SELECT pg_advisory_unlock($1, hashtext($2))`
	// sqlHoldIsOurs asks the server, not the client, whether THIS BACKEND still
	// holds the lock. Asking about the backend rather than merely running
	// `SELECT 1` is what catches a session-breaking deployment mistake:
	// a transaction-mode connection pooler in front of Postgres hands each
	// statement to whichever backend is free, so the lock would be taken on one
	// session, orphaned there for ever, and never actually held by the session
	// doing the work. This check answers NO in that deployment, and the work
	// stops instead of running unserialized.
	sqlHoldIsOurs = `SELECT EXISTS (
		SELECT 1 FROM pg_locks
		 WHERE locktype = 'advisory' AND granted
		   AND objsubid = 2 AND classid = $1::oid AND objid = hashtext($2)::oid
		   AND pid = pg_backend_pid())`
)

// dateHoldPoolConfig is the pool the holds live on: small, pre-warmed, and
// nothing like the work pool.
//
// Every value here is load-bearing.
//
//	MinConns == MaxConns   The connections exist BEFORE the pressure does. A
//	                       hold that had to dial under load would be a hold that
//	                       depends on the thing it is protecting against.
//	small                  One connection is checked out per concurrently held
//	                       DATE and is idle for the whole of it. Sizing is "how
//	                       many dispatch dates can one instance be writing at
//	                       once", which is a handful, not "how much traffic".
//	no lifetime recycling  pgxpool only recycles connections it has back, so
//	                       this cannot evict a live hold; it is set long anyway
//	                       so a hold connection is not churned for its age.
//	statement_timeout      The three statements a hold ever runs are a lock, an
//	                       unlock and an EXISTS. Seconds, not the work pool's 30.
//	tcp_keepalives_*       The server's own liveness check on the client. This is
//	                       what bounds how long a machine that vanished without
//	                       closing its sockets can hold a dispatch date: roughly
//	                       30s + 3x10s rather than the OS default of hours.
//	application_name       So `SELECT * FROM pg_stat_activity WHERE
//	                       application_name = 'ailm-date-holds'` answers "what is
//	                       holding a date right now", which is the first question
//	                       during an incident. Under the lease row that answer
//	                       came from a table; it must not simply be lost.
func dateHoldPoolConfig(conns int32) database.PoolConfig {
	if conns <= 0 {
		conns = defaultDateHoldConns
	}
	return database.PoolConfig{
		MaxConns:          conns,
		MinConns:          conns,
		MaxConnLifetime:   24 * time.Hour,
		MaxConnIdleTime:   24 * time.Hour,
		HealthCheckPeriod: 30 * time.Second,
		StatementTimeout:  dateHoldWait,
		IdleInTxTimeout:   dateHoldWait,
		Extra: map[string]string{
			"application_name":        "ailm-date-holds",
			"tcp_keepalives_idle":     "30",
			"tcp_keepalives_interval": "10",
			"tcp_keepalives_count":    "3",
		},
	}
}

// defaultDateHoldConns is how many dispatch dates one instance may hold at once,
// and it is the one number in this design that is a judgement rather than a
// consequence.
//
// It is chosen against the shape of the work, not against traffic: a hold exists
// only for the length of ONE write operation on ONE date, so this is "how many
// different dispatch DAYS can this dealer be writing in the same second", which
// is dispatchers-times-days-open and not requests-per-second. Sixteen is
// generous for that and still small against a managed Postgres's connection
// limit, which the work pool's 25 is already spending from.
//
// Being too small costs a truthful, retryable refusal (ErrDateHoldsFull) and
// never an unserialized write — the failure direction is the safe one. Being too
// large costs idle connections. DB_DATE_HOLD_CONNS moves it.
const defaultDateHoldConns int32 = 16

// DateHoldPoolConfig is dateHoldPoolConfig for cmd/server, which owns the dial.
func DateHoldPoolConfig(conns int32) database.PoolConfig { return dateHoldPoolConfig(conns) }

// pgDateHolds takes session-scoped advisory locks on a dedicated pool.
type pgDateHolds struct {
	pool *pgxpool.Pool
	// wait bounds the acquire: how long a writer may wait for a hold
	// connection before being refused. Zero means dateHoldWait.
	wait time.Duration
}

func (p pgDateHolds) acquire(ctx context.Context, date string) (heldDate, error) {
	if p.pool == nil {
		return nil, errors.New("no dispatch-date hold pool is configured")
	}
	wait := p.wait
	if wait <= 0 {
		wait = dateHoldWait
	}
	actx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	conn, err := p.pool.Acquire(actx)
	if err != nil {
		if ctx.Err() == nil && actx.Err() != nil {
			// Our own bound expired, not the caller's: every hold connection is
			// in use by some OTHER date.
			return nil, ErrDateHoldsFull
		}
		return nil, fmt.Errorf("take a dispatch-date hold connection: %w", err)
	}

	var got bool
	if err := conn.QueryRow(actx, sqlTakeDateHold, dateHoldClass, date).Scan(&got); err != nil {
		// The lock's state is unknown, so the SESSION goes rather than the
		// connection going back to the pool possibly holding it.
		discard(ctx, conn)
		return nil, err
	}
	if !got {
		// Definitively not ours: nothing was taken, so the connection is clean.
		conn.Release()
		return nil, nil
	}

	h := &pgHeldDate{conn: conn, date: date}
	// Confirm the lock is held by the backend we are keeping. This costs one
	// round-trip once per operation and it is what turns "a pooler in front of
	// Postgres silently disables this serialization" from a silent unserialized
	// deployment into a refusal on the first write.
	ours, err := h.stillOurs(actx)
	if err != nil {
		h.destroy(ctx)
		return nil, fmt.Errorf("confirm the dispatch-date hold for %s: %w", date, err)
	}
	if !ours {
		h.destroy(ctx)
		return nil, fmt.Errorf("took the dispatch-date hold for %s on a session that does not hold it — this database is reached through a transaction-pooling proxy, and a session-scoped lock cannot serialize anything through one", date)
	}
	return h, nil
}

// pgHeldDate is one held date: a session-scoped advisory lock and the checked-out
// connection whose session holds it.
type pgHeldDate struct {
	// mu is what makes "the hold's connection has exactly one user at a time" a
	// property of this type rather than of its caller. run already sequences
	// the watcher against the release; this makes a future caller that does not
	// still safe, because a pgx connection used concurrently corrupts the
	// protocol stream rather than returning an error.
	mu   sync.Mutex
	conn *pgxpool.Conn
	date string
}

func (h *pgHeldDate) stillOurs(ctx context.Context) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return false, nil
	}
	var ours bool
	if err := h.conn.QueryRow(ctx, sqlHoldIsOurs, dateHoldClass, h.date).Scan(&ours); err != nil {
		if h.conn.Conn().IsClosed() {
			// The session is gone and every lock it held went with it. That is
			// not "could not tell": it is a verified NO.
			return false, nil
		}
		return false, err
	}
	return ours, nil
}

func (h *pgHeldDate) release(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return
	}
	var dropped bool
	err := h.conn.QueryRow(ctx, sqlDropDateHold, dateHoldClass, h.date).Scan(&dropped)
	if err != nil || !dropped {
		// A connection whose lock state cannot be stated must never go back
		// into the pool: the next hold to be handed it would take the lock
		// re-entrantly, count it twice, and leave the date shut for the life of
		// the process. Ending the session releases everything it held.
		slog.Error("could not cleanly release a dispatch-date hold; ending its session instead",
			"date", h.date, "released", dropped, "error", err)
		discard(ctx, h.conn)
		h.conn = nil
		return
	}
	h.conn.Release()
	h.conn = nil
}

// destroy ends the session and returns the connection, for the acquire paths
// that must not keep it.
func (h *pgHeldDate) destroy(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return
	}
	discard(ctx, h.conn)
	h.conn = nil
}

// discard ends a connection's SESSION and hands it back. pgxpool destroys
// rather than reuses a connection that is closed, so this is what guarantees no
// pooled connection is ever holding an advisory lock nobody is tracking.
func discard(ctx context.Context, conn *pgxpool.Conn) {
	_ = conn.Conn().Close(context.WithoutCancel(ctx))
	conn.Release()
}
