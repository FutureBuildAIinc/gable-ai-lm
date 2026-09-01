// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/jackc/pgx/v5"
)

// The exclusive hold on one dispatch DATE.
//
// # Why a lease and not the transaction-scoped advisory lock it replaces
//
// The previous mechanism was pg_try_advisory_xact_lock, which is released by
// COMMIT or ROLLBACK. Holding a date therefore meant holding an open
// transaction for the whole operation, and an open transaction means one of
// MaxConns=25 pooled connections pinned across every GableLBM round-trip inside
// it — up to one 15s ERP timeout per truck on a push. The lock only serializes
// pushes for the SAME date, by design, so twenty-five slow pushes on twenty-five
// different dates were free to take twenty-five connections and starve every
// other endpoint in the process. The exclusion was correct; the cost was not
// survivable, and it was the very cost the design had used to argue against a
// blocking waiter.
//
// A lease row moves the hold out of the connection and into data. Acquire is
// one statement, release is one statement, and in between the connection is
// back in the pool. What that gives up is the advisory lock's one genuinely
// nice property — a dead process cannot wedge a date, because its transaction
// unwinds — and expires_at is what buys it back, at the cost of being a clock
// rather than a fact. Three things keep that honest:
//
//   - the holder RENEWS while it works, so a live holder never loses a date it
//     is still using and the TTL can be short enough (tens of seconds) that a
//     crashed holder frees the date quickly;
//   - a renewal that finds the lease is no longer ours CANCELS the work,
//     because somebody else is now entitled to write this date and two writers
//     is the entire failure this exists to prevent;
//   - the hold has a hard CEILING regardless of renewals, so a process that is
//     alive but wedged — an ERP that accepts the connection and never answers —
//     cannot hold a dispatch date open for ever either.
//
// # What it still does not do
//
// Nothing here makes the ERP write and the ledger write atomic. A process that
// dies between PushDeliveryRoute returning and the ledger being saved still
// leaves a route on the dealer's board that no ledger names. The lease bounds
// how long that date stays shut; it does not reconcile the orphan. That is the
// reconciler's job and it is not done here.

// dateLeaseStore is the three statements the hold is made of. It is an
// interface so the orchestration below — fail fast, renew, cancel on loss,
// release exactly once — can be tested without a Postgres, which is the only
// part of this file where a mistake is silent.
type dateLeaseStore interface {
	// acquire takes the date for holder until now()+ttl, and reports whether
	// it got it. It must be ONE atomic statement: two callers arriving together
	// must not both be told yes.
	acquire(ctx context.Context, date, holder string, ttl time.Duration) (bool, error)
	// renew extends holder's lease, and reports false if the lease is no
	// longer holder's (expired and taken, or released).
	renew(ctx context.Context, date, holder string, ttl time.Duration) (bool, error)
	// release drops the lease if and only if holder still owns it.
	release(ctx context.Context, date, holder string) error
}

// Production timings.
//
// ttl is short so a crashed holder frees its date fast; renewEvery is well
// under it so an ordinary GC pause or a slow renewal query cannot lose a lease
// the holder is still using; ceiling is the answer to "and if the process is
// alive but the ERP never replies?" — long enough for a real multi-truck push
// with slow round-trips, short enough that a dispatcher is not locked out of
// their day.
const (
	dateLeaseTTL     = 30 * time.Second
	dateLeaseRenew   = 8 * time.Second
	dateHoldCeiling  = 4 * time.Minute
	dateLeaseRelease = 5 * time.Second // release must still run when fn's ctx is done
)

// dateHold orchestrates one exclusive hold on a date.
type dateHold struct {
	leases      dateLeaseStore
	ttl         time.Duration
	renewEvery  time.Duration
	ceiling     time.Duration
	releaseWait time.Duration
}

func newDateHold(leases dateLeaseStore) dateHold {
	return dateHold{
		leases:      leases,
		ttl:         dateLeaseTTL,
		renewEvery:  dateLeaseRenew,
		ceiling:     dateHoldCeiling,
		releaseWait: dateLeaseRelease,
	}
}

// ErrDateHoldLost is returned when a hold was taken and then lost mid-flight —
// the lease expired and somebody else claimed the date, so this caller was
// stopped rather than allowed to keep writing a date it no longer owns.
//
// It is deliberately NOT ErrDateBusy: ErrDateBusy means nothing happened, and
// this means work was interrupted part-way. Reporting the second as the first
// would tell a dispatcher "nothing was sent" about a push that may already have
// put trucks on the board.
var ErrDateHoldLost = errors.New("lost the dispatch-date hold mid-flight")

// run takes the date, runs fn, and releases it.
//
// It FAILS FAST: a contended date returns ErrDateBusy having run nothing, which
// is what makes "try again" a plain repeat rather than a partial recovery.
//
// fn receives a context that is cancelled when the hold ends for any reason —
// the ceiling, a lost lease, or the caller's own context. Everything fn does
// downstream (ERP round-trips, repository writes) therefore stops with the
// hold instead of running on past it.
func (h dateHold) run(ctx context.Context, date string, fn func(ctx context.Context) error) error {
	holder, err := newHolderToken()
	if err != nil {
		return err
	}
	got, err := h.leases.acquire(ctx, date, holder, h.ttl)
	if err != nil {
		return fmt.Errorf("take the dispatch-date hold for %s: %w", date, err)
	}
	if !got {
		return ErrDateBusy
	}

	// The ceiling applies whatever the caller's own deadline is, so no request
	// can hold a dispatch date longer than this even if its client waits.
	work, cancel := context.WithTimeout(ctx, h.ceiling)
	defer cancel()

	stop := make(chan struct{})
	lost := make(chan struct{})
	go h.keepAlive(work, date, holder, stop, lost, cancel)

	err = fn(work)
	close(stop)

	// Release on a context DETACHED from work: work is already cancelled by the
	// deferred cancel above on every path, and a release that cannot run is a
	// date held until its TTL for no reason. It is still bounded.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), h.releaseWait)
	defer rcancel()
	if rerr := h.leases.release(rctx, date, holder); rerr != nil {
		// Not fatal to the caller's result: the lease expires on its own, and
		// reporting a bookkeeping failure over the push's own verdict is how a
		// dispatcher gets told the wrong thing.
		slog.Error("could not release a dispatch-date hold; it will expire on its own",
			"date", date, "ttl", h.ttl, "error", rerr)
	}

	// A hold lost mid-flight is reported ONLY when fn had nothing of its own to
	// say. Losing the hold cancels fn's context, so fn almost always returns
	// its own account of where it got to — "push stopped at truck 3, two trucks
	// are now live on the board" — and that account is what the dispatcher has
	// to act on. Replacing it with "the hold was lost" would trade the state of
	// the dealer's board for a description of this module's plumbing.
	select {
	case <-lost:
		if err == nil {
			return fmt.Errorf("%w: %s", ErrDateHoldLost, date)
		}
	default:
	}
	return err
}

// keepAlive extends the lease while fn runs, and cancels the work the moment
// the lease stops being ours.
//
// Losing the lease is not a warning to log — it means another writer is now
// entitled to this date, and continuing would be exactly the two-writer race
// the hold exists to prevent. So it cancels, and run reports it.
func (h dateHold) keepAlive(work context.Context, date, holder string, stop <-chan struct{}, lost chan<- struct{}, cancel context.CancelFunc) {
	t := time.NewTicker(h.renewEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-work.Done():
			return
		case <-t.C:
			held, err := h.leases.renew(work, date, holder, h.ttl)
			if err != nil {
				// A renewal that could not be attempted is not proof the lease
				// is gone; the next tick tries again, and the TTL is the
				// backstop if the database stays unreachable.
				slog.Warn("could not renew a dispatch-date hold", "date", date, "error", err)
				continue
			}
			if !held {
				slog.Error("lost a dispatch-date hold while work was in flight — stopping rather than writing a date somebody else now owns",
					"date", date)
				close(lost)
				cancel()
				return
			}
		}
	}
}

// newHolderToken mints the per-ACQUISITION identity release and renewal are
// conditioned on. Per acquisition, not per process: a process whose lease
// expired and was taken by somebody else must not be able to release or extend
// the new holder's lease just because it is still itself.
func newHolderToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint a dispatch-date hold token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// pgDateLeases is the Postgres implementation of dateLeaseStore.
type pgDateLeases struct{ db *database.DB }

// acquire is one statement on purpose. The upsert's WHERE is what makes it
// atomic: a row that has NOT expired fails the ON CONFLICT update, the
// statement returns no rows, and the caller is refused — there is no window
// between "read the row" and "claim it" for a second caller to fit into.
func (p pgDateLeases) acquire(ctx context.Context, date, holder string, ttl time.Duration) (bool, error) {
	var got string
	err := p.db.GetExecutor(ctx).QueryRow(ctx, `
		INSERT INTO workflow_date_leases (plan_date, holder, action, acquired_at, expires_at)
		VALUES ($1::date, $2, '', NOW(), NOW() + make_interval(secs => $3))
		ON CONFLICT (plan_date) DO UPDATE
		   SET holder = EXCLUDED.holder,
		       acquired_at = EXCLUDED.acquired_at,
		       expires_at = EXCLUDED.expires_at
		 WHERE workflow_date_leases.expires_at <= NOW()
		RETURNING holder`, date, holder, ttl.Seconds()).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == holder, nil
}

func (p pgDateLeases) renew(ctx context.Context, date, holder string, ttl time.Duration) (bool, error) {
	tag, err := p.db.GetExecutor(ctx).Exec(ctx, `
		UPDATE workflow_date_leases
		   SET expires_at = NOW() + make_interval(secs => $3)
		 WHERE plan_date = $1::date AND holder = $2`, date, holder, ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p pgDateLeases) release(ctx context.Context, date, holder string) error {
	_, err := p.db.GetExecutor(ctx).Exec(ctx,
		`DELETE FROM workflow_date_leases WHERE plan_date = $1::date AND holder = $2`, date, holder)
	return err
}
