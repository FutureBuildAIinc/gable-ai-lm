// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration for the AI_LM service. Values are
// sourced from environment variables with a godotenv fallback for local dev.
type Config struct {
	Port        string
	DatabaseURL string

	// Auth & Security — same convention as GableLBM-main / GableRun.
	// AuthMode "dev" disables JWT auth for local development; otherwise a token
	// verifier is required (fail-closed): SESSION_SECRET for AI_LM-issued staff
	// session tokens, and/or JWKS_URL for externally-issued tokens.
	AuthMode   string
	JWKSURL    string
	AuthIssuer string
	// SessionSecret signs/verifies AI_LM staff session JWTs (internal/auth +
	// pkg/middleware). Required when AUTH_MODE != "dev".
	SessionSecret string

	// GableLBM integration — AI_LM is a standalone service that pulls its
	// source-of-truth data (orders, products, vehicles, deliveries) from the
	// GableLBM ERP via /api/integration/* and writes approved routes back.
	GableAPIURL         string // e.g. http://localhost:8080
	GableIntegrationKey string // sent as X-Integration-Key

	// OpenRouteService (pillar 6: real OSS routing). When ORSAPIKey is set the
	// routing optimizer uses ORS's real road distance/duration matrix
	// (driving-hgv) instead of the haversine heuristic. Empty key ⇒ haversine
	// fallback (the service still runs, never hard-fails).
	ORSAPIKey  string // ORS_API_KEY
	ORSBaseURL string // ORS_BASE_URL (default https://api.openrouteservice.org)
	ORSProfile string // ORS_PROFILE (default driving-hgv — heavy lumber trucks)

	// OpenRouter LLM (pillar 6: single-key OSS inference). An OpenAI-compatible
	// chat client pointed at OpenRouter, defaulting to an open-weight model.
	// Empty key ⇒ AI features report "not configured" and degrade gracefully.
	OpenRouterAPIKey  string // OPENROUTER_API_KEY
	OpenRouterBaseURL string // OPENROUTER_BASE_URL (default https://openrouter.ai/api/v1)
	OpenRouterModel   string // OPENROUTER_MODEL (default an open-weight model id)

	// Depot — this install's yard/branch origin, used as the root of every
	// workflow route (sequencing, distances, durations, shift feasibility).
	// This is per-dealer data, never a code constant: an unset depot falls back
	// to the centroid of the day's stops, never to some other dealer's yard.
	// Both DEPOT_LAT and DEPOT_LNG must be supplied together (fail-closed).
	DepotLat *float64 // DEPOT_LAT
	DepotLng *float64 // DEPOT_LNG

	// Load securement (T1-5/T2-7). SecurementJurisdiction selects the cargo
	// tie-down ruleset (US_FMCSA, CA_NSC, …); SecurementAnchorSpacingIn models
	// the bed's tie-down anchor pitch so straps are optimized onto real anchors.
	SecurementJurisdiction    string  // SECUREMENT_JURISDICTION (default US_FMCSA)
	SecurementAnchorSpacingIn float64 // SECUREMENT_ANCHOR_SPACING_IN (default 24)

	// Scheduled re-optimization windows (T2-3). Times at which a plan's run
	// auto-locks against silent re-shuffles (HH:MM, 24h, plan-local).
	LockMorningAt   string // LOCK_MORNING_AT (default 06:00)
	LockAfternoonAt string // LOCK_AFTERNOON_AT (default 11:00)

	// Licensing / entitlement seam (internal/license). All three are optional:
	// with none of them set the deployment reports edition "evaluation", which
	// is what §1 of the OpenLBM Community Source License grants to everyone for
	// non-production use. It is NOT a lockout waiting to be switched on.
	//
	// A PRESENT token is verified offline and fails closed — see license.Load.
	// LicensePublicKeys holds the base64 Ed25519 VERIFICATION keys (plural
	// during a rotation); the signing private key never touches this service.
	LicenseToken      string // LICENSE_TOKEN — the token itself
	LicenseFile       string // LICENSE_FILE — path to a mounted token, used when LICENSE_TOKEN is unset
	LicensePublicKeys string // LICENSE_PUBLIC_KEY — comma/whitespace-separated base64 public keys

	// Logging
	LogLevel string // DEBUG, INFO, WARN, ERROR (default: INFO)

	// Database pool sizing (defaults mirror GableRun's PRR-tuned values).
	DBMaxConns        int32
	DBMinConns        int32
	DBMaxConnLifetime int // minutes
}

func Load() (*Config, error) {
	_ = godotenv.Load() // Load .env if present; ignore if not.

	depotLat, depotLng, err := loadDepot()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Port:        getEnv("PORT", "8090"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://gable_user:gable_password@localhost:5434/ai_lm_db?sslmode=disable"),

		AuthMode:      getEnv("AUTH_MODE", ""),
		JWKSURL:       getEnv("JWKS_URL", ""),
		AuthIssuer:    getEnv("AUTH_ISSUER", ""),
		SessionSecret: getEnv("SESSION_SECRET", ""),

		GableAPIURL:         getEnv("GABLE_API_URL", "http://localhost:8080"),
		GableIntegrationKey: getEnv("GABLE_INTEGRATION_KEY", ""),

		ORSAPIKey:  getEnv("ORS_API_KEY", ""),
		ORSBaseURL: getEnv("ORS_BASE_URL", "https://api.openrouteservice.org"),
		ORSProfile: getEnv("ORS_PROFILE", "driving-hgv"),

		OpenRouterAPIKey:  getEnv("OPENROUTER_API_KEY", ""),
		OpenRouterBaseURL: getEnv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		OpenRouterModel:   getEnv("OPENROUTER_MODEL", "meta-llama/llama-3.3-70b-instruct"),

		DepotLat: depotLat,
		DepotLng: depotLng,

		SecurementJurisdiction:    getEnv("SECUREMENT_JURISDICTION", "US_FMCSA"),
		SecurementAnchorSpacingIn: getEnvFloat("SECUREMENT_ANCHOR_SPACING_IN", 24),

		LockMorningAt:   getEnv("LOCK_MORNING_AT", "06:00"),
		LockAfternoonAt: getEnv("LOCK_AFTERNOON_AT", "11:00"),

		LicenseToken:      getEnv("LICENSE_TOKEN", ""),
		LicenseFile:       getEnv("LICENSE_FILE", ""),
		LicensePublicKeys: getEnv("LICENSE_PUBLIC_KEY", ""),

		LogLevel: getEnv("LOG_LEVEL", "INFO"),

		DBMaxConns:        int32(getEnvInt("DB_MAX_CONNS", 25)),
		DBMinConns:        int32(getEnvInt("DB_MIN_CONNS", 2)),
		DBMaxConnLifetime: getEnvInt("DB_MAX_CONN_LIFETIME_MIN", 60),
	}

	// In non-dev mode, DATABASE_URL must be explicitly set — the localhost
	// fallback is only safe for local development.
	if cfg.AuthMode != "dev" {
		if _, explicit := os.LookupEnv("DATABASE_URL"); !explicit {
			return nil, fmt.Errorf("FATAL: DATABASE_URL must be explicitly set when AUTH_MODE != 'dev' (refusing to fall back to localhost)")
		}
		// DigitalOcean managed-DB bindings (${db.DATABASE_URL}) connect over TLS
		// but omit an explicit sslmode query param. Rather than fail closed,
		// enforce it by appending sslmode=require when no sslmode is present.
		// If an sslmode IS present but is insecure, that's a deliberate
		// misconfiguration and we still refuse.
		if !strings.Contains(cfg.DatabaseURL, "sslmode=") {
			sep := "?"
			if strings.Contains(cfg.DatabaseURL, "?") {
				sep = "&"
			}
			cfg.DatabaseURL += sep + "sslmode=require"
		} else if !strings.Contains(cfg.DatabaseURL, "sslmode=require") &&
			!strings.Contains(cfg.DatabaseURL, "sslmode=verify-full") &&
			!strings.Contains(cfg.DatabaseURL, "sslmode=verify-ca") {
			return nil, fmt.Errorf("FATAL: DATABASE_URL has an insecure sslmode; require/verify-full/verify-ca needed when AUTH_MODE != 'dev'")
		}
		// AI_LM now mints its own staff session tokens (internal/auth), so
		// SESSION_SECRET is the required verifier in production. JWKS_URL is
		// optional and only needed to additionally accept externally-issued
		// (shared GableLBM JWKS) tokens.
		if cfg.SessionSecret == "" {
			return nil, fmt.Errorf("FATAL: SESSION_SECRET must be set when AUTH_MODE != 'dev' (signs AI_LM staff session tokens)")
		}
	}

	return cfg, nil
}

// loadDepot reads this install's yard/branch origin from DEPOT_LAT/DEPOT_LNG.
// It is fail-closed in the same spirit as the DATABASE_URL/SESSION_SECRET
// checks: a malformed, out-of-range, or half-supplied depot is a hard boot
// error rather than a silently-wrong routing origin that would root every
// route at (0,0) or at some other dealer's yard. Both unset ⇒ (nil, nil), and
// the workflow falls back to the centroid of the day's own stops.
func loadDepot() (*float64, *float64, error) {
	rawLat, hasLat := os.LookupEnv("DEPOT_LAT")
	rawLng, hasLng := os.LookupEnv("DEPOT_LNG")
	if !hasLat && !hasLng {
		return nil, nil, nil
	}
	if !hasLat || !hasLng {
		return nil, nil, fmt.Errorf("FATAL: DEPOT_LAT and DEPOT_LNG must be set together (got lat=%t lng=%t)", hasLat, hasLng)
	}

	lat, err := strconv.ParseFloat(strings.TrimSpace(rawLat), 64)
	if err != nil {
		return nil, nil, fmt.Errorf("FATAL: DEPOT_LAT %q is not a number: %w", rawLat, err)
	}
	lng, err := strconv.ParseFloat(strings.TrimSpace(rawLng), 64)
	if err != nil {
		return nil, nil, fmt.Errorf("FATAL: DEPOT_LNG %q is not a number: %w", rawLng, err)
	}
	if lat < -90 || lat > 90 {
		return nil, nil, fmt.Errorf("FATAL: DEPOT_LAT %v is out of range (-90..90)", lat)
	}
	if lng < -180 || lng > 180 {
		return nil, nil, fmt.Errorf("FATAL: DEPOT_LNG %v is out of range (-180..180)", lng)
	}
	return &lat, &lng, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value, exists := os.LookupEnv(key); exists {
		n, err := strconv.Atoi(value)
		if err != nil {
			slog.Warn("Invalid integer env var, using default", "key", key, "value", value, "default", fallback)
			return fallback
		}
		return n
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if value, exists := os.LookupEnv(key); exists {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			slog.Warn("Invalid float env var, using default", "key", key, "value", value, "default", fallback)
			return fallback
		}
		return f
	}
	return fallback
}
