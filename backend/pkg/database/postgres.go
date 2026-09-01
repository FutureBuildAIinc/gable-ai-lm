// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package database

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Executor is an interface that matches both pgxpool.Pool and pgx.Tx.
type Executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type DB struct {
	Pool *pgxpool.Pool
}

// PoolConfig holds configurable pool parameters.
type PoolConfig struct {
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration

	// StatementTimeout and IdleInTxTimeout are the two server-side ceilings
	// that stop ONE request holding a pooled connection for ever.
	//
	// MaxConns is 25 for the whole service. Without these, a single query that
	// never returns — a lock wait behind another session, a plan that went
	// quadratic, a network path that black-holes rather than resets — parks a
	// connection indefinitely, and 25 of them park the pool for every endpoint
	// in the process, including /health. There was no upper bound of any kind
	// on either before this: `grep statement_timeout` over this repository
	// returned nothing.
	//
	// IdleInTxTimeout is the sharper of the two, because "idle in transaction"
	// is precisely the shape a BEGIN held across a call to somebody else's HTTP
	// API takes. This module's dispatch-date hold deliberately does not do that
	// any more (see internal/workflow's date lease); this setting is what makes
	// that a property of the deployment rather than of one function that could
	// be rewritten tomorrow.
	//
	// Zero means "use the package default". It is deliberately not "no limit":
	// the pre-existing caller in cmd/server builds a PoolConfig literal, and a
	// zero value there must not silently opt the whole service out of the only
	// bound it has.
	StatementTimeout time.Duration
	IdleInTxTimeout  time.Duration
}

// Default statement / transaction ceilings. They are minutes rather than
// seconds because the honest worst case for a legitimate query here (the
// ListForDate behind a re-plan of a busy day) is slow, not unbounded — the job
// of these values is to catch the wedged case, not to police the slow one.
const (
	defaultStatementTimeout = 30 * time.Second
	defaultIdleInTxTimeout  = 60 * time.Second
)

// DefaultPoolConfig returns sensible defaults for the connection pool.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxConns:          25,
		MinConns:          2,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   30 * time.Minute,
		HealthCheckPeriod: 1 * time.Minute,
		StatementTimeout:  defaultStatementTimeout,
		IdleInTxTimeout:   defaultIdleInTxTimeout,
	}
}

// poolConfigFor builds the pgx pool configuration Connect dials with.
//
// It is split out of Connect so the wiring can be asserted without a database:
// Connect's remaining body is a dial, a ping and error mapping, and everything
// that could silently stop being applied — the pool sizes, and the two
// server-side timeouts that bound how long one request may hold a connection —
// is decided here.
func poolConfigFor(connString string, pc PoolConfig) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("unable to parse connection string: %w", err)
	}
	config.MaxConns = pc.MaxConns
	config.MinConns = pc.MinConns
	config.MaxConnLifetime = pc.MaxConnLifetime
	config.MaxConnIdleTime = pc.MaxConnIdleTime
	config.HealthCheckPeriod = pc.HealthCheckPeriod

	st := pc.StatementTimeout
	if st <= 0 {
		st = defaultStatementTimeout
	}
	idle := pc.IdleInTxTimeout
	if idle <= 0 {
		idle = defaultIdleInTxTimeout
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	// Sent as connection startup parameters, so they apply to every session
	// the pool opens — including ones opened later to replace a recycled
	// connection — rather than to whichever session happened to run a SET.
	config.ConnConfig.RuntimeParams["statement_timeout"] = millis(st)
	config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = millis(idle)
	return config, nil
}

// millis renders a duration as the integer milliseconds Postgres expects for
// these GUCs (a bare number is milliseconds for both).
func millis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

func Connect(connString string, opts ...PoolConfig) (*DB, error) {
	pc := DefaultPoolConfig()
	if len(opts) > 0 {
		pc = opts[0]
	}
	config, err := poolConfigFor(connString, pc)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("unable to ping database: %w", err)
	}

	return &DB{Pool: pool}, nil
}

func (db *DB) Close() {
	db.Pool.Close()
}

// RunInTx executes a function within a database transaction.
//
// The return value is NAMED, and that is load-bearing rather than stylistic.
// The deferred block below is what commits, so it runs AFTER the return value
// has been fixed; with an unnamed result, `err = tx.Commit(ctx)` assigned to a
// local nothing would ever read and a transaction that FAILED TO COMMIT was
// reported to the caller as success. For a caller that writes a ledger
// recording what it has already sent to another system, that is the exact
// shape of the harm the ledger exists to prevent: the remote write stands, the
// local record of it is rolled back, and the caller is told it worked. A
// commit error is now returned like any other.
func (db *DB) RunInTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, beginErr := db.Pool.Begin(ctx)
	if beginErr != nil {
		return fmt.Errorf("failed to begin transaction: %w", beginErr)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			err = fmt.Errorf("commit transaction: %w", cerr)
		}
	}()

	ctxWithTx := context.WithValue(ctx, txKey{}, tx)
	err = fn(ctxWithTx)
	return err
}

type txKey struct{}

// GetExecutor returns the active transaction if one is in the context,
// otherwise the pool.
func (db *DB) GetExecutor(ctx context.Context) Executor {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return db.Pool
}
