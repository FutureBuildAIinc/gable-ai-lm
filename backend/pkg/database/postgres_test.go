// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package database

import (
	"testing"
	"time"
)

const testDSN = "postgres://ailm:secret@db.internal:5432/ailm?sslmode=disable"

// TestEverySessionCarriesAStatementCeiling is the pool's only defence against
// one request holding a connection for ever.
//
// MaxConns is 25 for the whole service, shared by every endpoint including
// /health. Before this there was no ceiling of any kind — `grep
// statement_timeout` over this repository returned nothing — so a single query
// that never returned parked a connection indefinitely and twenty-five of them
// parked the service.
//
// idle_in_transaction_session_timeout is the sharper of the two, because "idle
// in transaction" is exactly the shape a BEGIN held across somebody else's HTTP
// API takes. The dispatch-date hold deliberately no longer does that; this
// setting is what makes that a property of the deployment rather than of one
// function anybody could rewrite tomorrow.
func TestEverySessionCarriesAStatementCeiling(t *testing.T) {
	cfg, err := poolConfigFor(testDSN, DefaultPoolConfig())
	if err != nil {
		t.Fatalf("poolConfigFor: %v", err)
	}
	for _, param := range []string{"statement_timeout", "idle_in_transaction_session_timeout"} {
		got := cfg.ConnConfig.RuntimeParams[param]
		if got == "" {
			t.Errorf("%s is unset — one wedged query can hold a pooled connection for ever", param)
			continue
		}
		if got == "0" {
			t.Errorf("%s is 0, which Postgres reads as NO LIMIT", param)
		}
	}
	if want := "30000"; cfg.ConnConfig.RuntimeParams["statement_timeout"] != want {
		t.Errorf("statement_timeout = %q, want %q milliseconds", cfg.ConnConfig.RuntimeParams["statement_timeout"], want)
	}
	if want := "60000"; cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] != want {
		t.Errorf("idle_in_transaction_session_timeout = %q, want %q milliseconds", cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"], want)
	}
}

// TestAZeroValuedPoolConfigStillGetsTheCeilings is the case that actually
// ships.
//
// cmd/server does not call DefaultPoolConfig — it builds a PoolConfig literal
// from environment settings, and that literal names five fields. A new field
// defaulting to zero there must not silently opt the whole service out of the
// only bound it has, which is what "zero means no limit" would have done.
func TestAZeroValuedPoolConfigStillGetsTheCeilings(t *testing.T) {
	asShipped := PoolConfig{
		MaxConns:          25,
		MinConns:          2,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   30 * time.Minute,
		HealthCheckPeriod: time.Minute,
		// StatementTimeout and IdleInTxTimeout deliberately unset.
	}
	cfg, err := poolConfigFor(testDSN, asShipped)
	if err != nil {
		t.Fatalf("poolConfigFor: %v", err)
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "30000" {
		t.Errorf("statement_timeout = %q — a caller that omits the field must inherit the default, not disable it", got)
	}
	if got := cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"]; got != "60000" {
		t.Errorf("idle_in_transaction_session_timeout = %q — a caller that omits the field must inherit the default, not disable it", got)
	}
}

// TestPoolConfigForCarriesTheCallersSizes keeps the extraction honest: the
// timeouts are new, everything the pool already did must still happen.
func TestPoolConfigForCarriesTheCallersSizes(t *testing.T) {
	in := PoolConfig{
		MaxConns:          7,
		MinConns:          3,
		MaxConnLifetime:   11 * time.Minute,
		MaxConnIdleTime:   4 * time.Minute,
		HealthCheckPeriod: 90 * time.Second,
		StatementTimeout:  2 * time.Second,
		IdleInTxTimeout:   5 * time.Second,
	}
	cfg, err := poolConfigFor(testDSN, in)
	if err != nil {
		t.Fatalf("poolConfigFor: %v", err)
	}
	if cfg.MaxConns != 7 || cfg.MinConns != 3 {
		t.Errorf("pool sizes = %d/%d, want 7/3", cfg.MaxConns, cfg.MinConns)
	}
	if cfg.MaxConnLifetime != in.MaxConnLifetime || cfg.MaxConnIdleTime != in.MaxConnIdleTime || cfg.HealthCheckPeriod != in.HealthCheckPeriod {
		t.Errorf("lifetimes not carried: %+v", cfg)
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "2000" {
		t.Errorf("statement_timeout = %q, want 2000", got)
	}
	if got := cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"]; got != "5000" {
		t.Errorf("idle_in_transaction_session_timeout = %q, want 5000", got)
	}
}

// TestPoolConfigForRejectsAnUnparseableDSN keeps the error path from silently
// producing a config with no connection details.
func TestPoolConfigForRejectsAnUnparseableDSN(t *testing.T) {
	if _, err := poolConfigFor("://not-a-dsn", DefaultPoolConfig()); err == nil {
		t.Fatal("an unparseable connection string must be an error")
	}
}
