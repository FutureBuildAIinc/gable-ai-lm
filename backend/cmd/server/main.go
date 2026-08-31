// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/ai"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/auth"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/catalog"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/config"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/fleet"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/license"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/load"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/routing"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/workflow"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/metrics"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	startTime := time.Now()

	// 1. Structured logging (JSON) with configurable level.
	logLevel := new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	// 2. Config.
	cfg, err := config.Load()
	if err != nil {
		logger.Error("Configuration error", "error", err)
		os.Exit(1)
	}
	switch strings.ToUpper(cfg.LogLevel) {
	case "DEBUG":
		logLevel.Set(slog.LevelDebug)
	case "WARN":
		logLevel.Set(slog.LevelWarn)
	case "ERROR":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}

	if !strings.EqualFold(cfg.AuthMode, "dev") && os.Getenv("CORS_ORIGINS") == "" {
		logger.Error("CORS_ORIGINS not set and AUTH_MODE != dev; set CORS_ORIGINS for production or AUTH_MODE=dev for development")
		os.Exit(1)
	}

	logger.Info("Starting AI_LM server...", "port", cfg.Port, "auth_mode", cfg.AuthMode, "log_level", cfg.LogLevel)

	// 2b. License / entitlement seam (internal/license). Loaded FIRST, before
	// the database and before anything binds a port, because a present-but-
	// invalid token is a hard startup failure and there is nothing to be gained
	// by discovering that after opening a connection pool.
	//
	// Three outcomes, and only the first stops the service:
	//   - a token that will not verify, or one acknowledging Competing Use →
	//     Load returns an error and we exit. Never a silent downgrade: a
	//     signature nobody enforces is decorative.
	//   - a token that has EXPIRED → a WARN naming the date, edition drops to
	//     "evaluation", and the service boots. An expiry must not take a
	//     dealer's dispatch offline mid-shift.
	//   - no token at all → edition "evaluation". That is §1 of the OpenLBM
	//     Community Source License operating as written (non-production use is
	//     granted to everyone), not a gap to be closed.
	lic, err := license.Load(license.Config{
		Token:      cfg.LicenseToken,
		File:       cfg.LicenseFile,
		PublicKeys: license.SplitKeys(cfg.LicensePublicKeys),
	})
	if err != nil {
		logger.Error("License validation failed — refusing to start", "error", err)
		os.Exit(1)
	}
	for _, w := range lic.Warnings() {
		logger.Warn(w)
	}
	// The one licence line. Everything an operator needs to answer "what is
	// this instance entitled to, and until when" is on it.
	logger.Info("License loaded", lic.LogAttrs()...)
	// No handler consults lic.Allow in v1 and that is the design: the seam
	// gates NOTHING. When a capability does become commercial-only, `lic` is
	// already here to be passed to the module that owns it, and the gate is one
	// call rather than a retrofit through a shipped system.

	// 3. Database.
	db, err := database.Connect(cfg.DatabaseURL, database.PoolConfig{
		MaxConns:          cfg.DBMaxConns,
		MinConns:          cfg.DBMinConns,
		MaxConnLifetime:   time.Duration(cfg.DBMaxConnLifetime) * time.Minute,
		MaxConnIdleTime:   30 * time.Minute,
		HealthCheckPeriod: 1 * time.Minute,
	})
	if err != nil {
		logger.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	logger.Info("Connected to database")

	// 3b. Prometheus metrics.
	metrics.Register()
	metricsCtx, metricsCancel := context.WithCancel(context.Background())
	defer metricsCancel()
	metrics.StartDBPoolCollector(metricsCtx, db.Pool, 15*time.Second)
	// Business metering, labelled with this deployment's licence identity. One
	// meter per process: with one stack per dealer the deployment IS the
	// tenant, so there is no per-request resolution and no tenancy model to
	// build. Constructing it here also materialises all four ailm_* series at
	// zero, so they are on /metrics from the first scrape.
	meter := metrics.NewMeter(string(lic.Edition()), lic.Subject())
	logger.Info("Prometheus metrics initialized", "metering_edition", string(lic.Edition()), "metering_subject", lic.Subject())

	// 4. Auth middleware — fail-closed: a token verifier (SESSION_SECRET and/or
	// JWKS_URL) is required unless AUTH_MODE=dev. /api/v1/auth/login is public so
	// staff can obtain a session before they hold a token.
	var authMw *middleware.AuthMiddleware
	if strings.EqualFold(cfg.AuthMode, "dev") {
		logger.Warn("AUTH_MODE=dev: authentication disabled (development only)")
	} else {
		logger.Info("Initializing Auth Middleware", "jwks_url", cfg.JWKSURL, "session_tokens", cfg.SessionSecret != "")
		am, err := middleware.NewAuthMiddleware(context.Background(), middleware.AuthConfig{
			JWKSURL:       cfg.JWKSURL,
			Issuer:        cfg.AuthIssuer,
			SessionSecret: cfg.SessionSecret,
			PublicPaths:   []string{"/health", "/healthz/live", "/healthz/ready", "/metrics", "/api/v1/auth/login"},
		}, logger)
		if err != nil {
			logger.Error("Failed to initialize Auth Middleware", "error", err)
			os.Exit(1)
		}
		authMw = am
	}

	// 5. GableLBM integration client.
	gableClient := gable.NewClient(cfg.GableAPIURL, cfg.GableIntegrationKey)
	logger.Info("GableLBM integration client initialized", "base_url", cfg.GableAPIURL)

	// 5a. Pillar 6 — OSS inference.
	// Real OSS routing: install the OpenRouteService-backed optimizer behind the
	// routing seam. With no ORS_API_KEY the provider transparently falls back to
	// the haversine heuristic, so the service runs unchanged.
	orsProvider := routing.NewORSProvider(cfg.ORSAPIKey, cfg.ORSBaseURL, cfg.ORSProfile)
	routing.SetOptimizer(orsProvider)
	if orsProvider.Configured() {
		logger.Info("OpenRouteService routing enabled", "profile", cfg.ORSProfile, "base_url", cfg.ORSBaseURL)
	} else {
		logger.Warn("ORS_API_KEY not set — routing uses the haversine heuristic (set ORS_API_KEY for real road-matrix routing)")
	}

	// Single-key OpenRouter LLM client (open-weight default model). Always
	// constructed; reports "not configured" when OPENROUTER_API_KEY is unset so
	// the AI dispatch briefing degrades gracefully.
	aiClient := ai.NewClient(cfg.OpenRouterAPIKey, cfg.OpenRouterBaseURL, cfg.OpenRouterModel)
	if aiClient.Configured() {
		logger.Info("OpenRouter LLM client enabled", "model", aiClient.Model())
	} else {
		logger.Warn("OPENROUTER_API_KEY not set — AI dispatch briefings unavailable (set OPENROUTER_API_KEY to enable)")
	}

	// 6. Router & modules.
	mux := http.NewServeMux()

	writeGuard := middleware.RequireRole("admin", "owner", "dispatcher", "yard")

	// Staff auth (pillar 4): public login backed by GableLBM validate-staff;
	// mints AI_LM session tokens. Registered without the writeGuard.
	authSvc := auth.NewService(gableClient, cfg.SessionSecret)
	auth.NewHandler(authSvc).RegisterRoutes(mux)

	// Fleet (vehicle profiles).
	fleetSvc := fleet.NewService(fleet.NewRepository(db))
	fleet.NewHandler(fleetSvc).RegisterRoutes(mux, writeGuard)

	// Catalog (product dimensions + PIM-merged effective geometry).
	catalogSvc := catalog.NewService(catalog.NewRepository(db), gableClient).WithMeter(meter)
	catalog.NewHandler(catalogSvc).RegisterRoutes(mux, writeGuard)

	// Load optimization (pillar 1).
	loadSvc := load.NewService(load.NewRepository(db), fleetSvc, load.NewTieredSolver(),
		cfg.SecurementJurisdiction, cfg.SecurementAnchorSpacingIn)
	load.NewHandler(loadSvc).RegisterRoutes(mux, writeGuard)

	// Routing (pillar 2). It is handed the branch list and this install's
	// configured depot for the same reason the workflow is: both root their
	// runs through the one ladder in internal/depot, and an unwired dependency
	// there is a nil panic on the first plan that names a branch.
	routingSvc := routing.NewService(routing.NewRepository(db), gableClient, gableClient, gableClient, gableClient, gableClient,
		routing.Config{DepotLat: cfg.DepotLat, DepotLng: cfg.DepotLng})
	routing.NewHandler(routingSvc).RegisterRoutes(mux, writeGuard)

	// Compliance (pillar 2).
	complianceSvc := compliance.NewService(compliance.NewRepository(db))
	compliance.NewHandler(complianceSvc).RegisterRoutes(mux, writeGuard)

	// Guided end-to-end workflow: ingest → analyze → assign → pack → review → push.
	workflowSvc := workflow.NewService(workflow.NewRepository(db), gableClient, catalogSvc, fleetSvc, complianceSvc, aiClient,
		workflow.Config{
			SecurementJurisdiction:    cfg.SecurementJurisdiction,
			SecurementAnchorSpacingIn: cfg.SecurementAnchorSpacingIn,
			LockMorningAt:             cfg.LockMorningAt,
			LockAfternoonAt:           cfg.LockAfternoonAt,
			DepotLat:                  cfg.DepotLat,
			DepotLng:                  cfg.DepotLng,
		})
	workflowSvc.WithMeter(meter)
	workflow.NewHandler(workflowSvc).RegisterRoutes(mux, writeGuard)
	// DEPOT_LAT/DEPOT_LNG are the install-wide fallback, shared by the workflow
	// and routing modules because both resolve their origin through the one
	// ladder in internal/depot. A plan prefers the GableLBM branch it actually
	// ships from, so this is what it lands on when no branch is usable.
	if cfg.DepotLat != nil && cfg.DepotLng != nil {
		logger.Info("Fallback depot configured", "lat", *cfg.DepotLat, "lng", *cfg.DepotLng)
	} else {
		logger.Warn("DEPOT_LAT/DEPOT_LNG not set — a plan that cannot root at its GableLBM branch falls back to the centroid of that day's stops (set them to this yard's coordinates)")
	}

	// Health — liveness.
	mux.HandleFunc("GET /healthz/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Health — readiness (checks DB).
	mux.HandleFunc("GET /healthz/ready", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		httpStatus := http.StatusOK
		dbStatus := "connected"
		if err := db.Pool.Ping(r.Context()); err != nil {
			status = "degraded"
			httpStatus = http.StatusServiceUnavailable
			dbStatus = "disconnected"
			logger.Error("Readiness check failed", "error", err)
		}
		poolStat := db.Pool.Stat()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": status,
			"uptime": time.Since(startTime).String(),
			"checks": map[string]interface{}{
				"database": map[string]interface{}{
					"status":      dbStatus,
					"pool_total":  poolStat.TotalConns(),
					"pool_idle":   poolStat.IdleConns(),
					"pool_in_use": poolStat.AcquiredConns(),
					"pool_max":    poolStat.MaxConns(),
				},
			},
		})
	})

	// Legacy /health endpoint.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		dbStatus := "connected"
		if err := db.Pool.Ping(r.Context()); err != nil {
			status = "error"
			dbStatus = "disconnected"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": status, "db": dbStatus})
	})

	// Prometheus scrape target.
	mux.Handle("GET /metrics", promhttp.Handler())

	// 7. Middleware chain (outermost first when reading bottom-up).
	var finalHandler http.Handler = mux
	if authMw != nil {
		finalHandler = authMw.Handler(finalHandler)
	}
	finalHandler = middleware.CORSMiddleware(finalHandler)
	finalHandler = middleware.Recovery(logger)(finalHandler)
	finalHandler = middleware.RequestID(finalHandler)
	finalHandler = metrics.HTTPMetrics(finalHandler)

	// 8. Server with graceful shutdown.
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.Port),
		Handler:           finalHandler,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Server failed", "error", err)
			os.Exit(1)
		}
	}()
	logger.Info("AI_LM server listening", "addr", srv.Addr)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("Shutdown signal received", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger.Info("Shutdown step 1/3: draining HTTP connections...")
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("HTTP server forced to shutdown", "error", err)
		os.Exit(1)
	}
	logger.Info("Shutdown step 2/3: stopping metrics collector...")
	metricsCancel()
	logger.Info("Shutdown step 3/3: closing database pool...")
	db.Close()
	logger.Info("Server exiting — clean shutdown complete")
}
