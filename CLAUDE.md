<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What Is This?

**AI_LM** (AI Load Management & Compliance Module) is a standalone microservice for
independent lumber & building-materials (LBM) dealers running on the **GableLBM** ERP.
It tackles two costly problems: aggressive truck weight enforcement (axle/GVW fines) and
subjective, manual dispatch. It does so with two pillars:

1. **Load Optimization** — a 3D visual model (Three.js) of a selected truck/trailer's
   load that auto-calculates material placement to balance weight across axles and gives
   yard staff clear loading guidance.
2. **Regulatory Compliance & Routing** — daily-order routing that returns a pre-optimized
   (80–90% complete) route + load config for dispatcher fine-tuning, with **GVW
   enforcement** during load-building and **restricted-point flagging** (weight/height/
   width-limited bridges & overpasses) along the route.

**Strategic intent:** a community-owned open-source core for GableLBM, architected as a
**standalone service** so it can later be commercially licensed to third-party ERPs via a
clean integration API.

## Relationship to GableLBM

AI_LM is a **standalone service + integration API** — its own backend, own Postgres DB,
own Lit UI. It does **not** share a process or schema with GableLBM. It pulls the source
of truth (orders, products+weight, vehicles, delivery geo) from GableLBM via
`GET /api/integration/*` using an `X-Integration-Key`, and writes approved routes back via
`POST /api/integration/delivery-routes`.

**Why AI_LM keeps its own fleet/catalog dimension data:** GableLBM products carry
`weight_lbs` but no L/W/H; vehicles carry `capacity_weight_lbs` but no axle config or bed
dimensions. Rather than mutate the ERP schema (which would hurt portability to other
ERPs), AI_LM stores these supplementary attributes itself, keyed by GableLBM UUIDs, with
sensible UOM-based defaults. This is what makes the module portable for commercial
licensing.

The Go module path is `github.com/futurebuildai/ai-lm`.

## Repo Structure

```
app/          → Lit 3 frontend (Vite + TypeScript + Tailwind + Three.js + Leaflet)
backend/      → Go backend (stdlib http.ServeMux + pgx + PostgreSQL)
.do/          → Digital Ocean App Platform manifests
docker-compose.yml  → Local Postgres (:5434, db ai_lm_db)
```

### Companion docs
- `ARCHITECTURE.md` — module map, request flow, the digital-twin pipeline, design decisions.
- `INTEGRATIONS.md` — the GableLBM contract this service consumes (and how to swap the backend).
- `DEVOPS.md` — deployment source-of-truth: DO apps, `doctl` runbook, app IDs, migrate/seed, rollback.

## Tech Stack

Mirrors GableLBM/GableRun exactly for cross-repo consistency.

### Backend
- **Language:** Go 1.25 (`backend/go.mod`, module `github.com/futurebuildai/ai-lm`)
- **Router:** Go 1.22+ stdlib `net/http.ServeMux`. Modules expose
  `RegisterRoutes(mux, guard ...func(http.Handler) http.Handler)`.
- **Database:** PostgreSQL 16+ via pgx v5 (`pkg/database` wraps `*pgxpool.Pool`)
- **Auth:** JWT verified against JWKS (`pkg/middleware.NewAuthMiddleware`). `AUTH_MODE=dev`
  disables auth for local dev; otherwise `JWKS_URL` is required (fail-closed). Share the
  GableLBM JWKS in production.
- **Metrics:** Prometheus at `/metrics`; DB pool collector.
- **Port:** **8090** (avoids GableLBM :8080 and Vite :5173).

### Frontend
- **Framework:** Lit 3 Web Components + TypeScript 5.9 + Vite 7
- **Styling:** Tailwind CSS 3.4 + Industrial Dark tokens
- **Components:** Custom **`ailm-`** prefixed web components, **Light DOM**
  (`createRenderRoot() { return this; }`) so Tailwind utilities apply
- **Routing:** Custom SPA router in `app/src/lib/router.ts`. Route table is
  `app/src/routes.ts`.
- **3D:** Three.js (`<ailm-load-3d-visualizer>`) | **Maps:** Leaflet
  (`<ailm-route-map>`, dark CARTO tiles) | **Charts:** Chart.js | **Icons:** Lucide via
  `lib/icons.ts`
- **HTTP:** `services/fetchClient.ts` (`fetchWithAuth`) + `services/aiLmService.ts`
  (typed client). Never call `fetch` directly from pages. The app uses relative
  `/api/v1/*` paths; Vite proxies them to `:8090` in dev, nginx reverse-proxies in prod.

## Architecture

```
GableLBM (source of truth)            AI_LM
  /api/integration/products   ─────▶  internal/gable  (X-Integration-Key client)
  /api/integration/vehicles   ─────▶  internal/fleet  (axle/bed profiles by gable id)
  /api/integration/orders     ─────▶  internal/catalog(product dims; weight from LBM)
  /api/integration/             ◀───  internal/load   (3D placement + axle/GVW solver)
       delivery-routes (write-back)   internal/routing(daily-order optimizer)
                                      internal/compliance (GVW rules + restricted points)
```

- **Pattern:** Modular monolith — single Go binary, modules under `backend/internal/`.
- **Module shape:** `model.go`, `repository.go` (pgx), `service.go`, `handler.go`. Wired
  in `backend/cmd/server/main.go`.
- **`internal/workflow` is the orchestrator** for the guided end-to-end flow
  (ingest → analyze → assign → pack → review → push). It reuses the other
  modules through narrow interfaces (gable client, catalog resolver, fleet
  profiles, compliance checker) and persists each run as one JSONB document in
  `workflow_plans` so the UI can resume any stage. Truck assignment is a
  sweep-angle CVRP (`assign.go`: 80% cargo cap, max 3 stops/truck, shift-length
  cap); compliance review (`review.go`) auto-resolves FAIL flags by inserting
  detour waypoints, re-packing under a height cap, or moving stops to another
  truck (activating an idle vehicle if needed).
- **Solver/optimizer are deterministic heuristics behind interfaces** (`load.Solver`,
  nearest-neighbor + 2-opt routing, haversine restricted-point buffering) so an
  AI/optimizer can replace them later without touching callers.
- **API surface:** REST JSON at `/api/v1/*`; public `/health`, `/healthz/live`,
  `/healthz/ready`, `/metrics`.

### API endpoints (`/api/v1/*`)

| Method | Path | Module |
|--------|------|--------|
| GET    | `/fleet/profiles`                       | fleet |
| GET/PUT| `/fleet/profiles/{vehicleId}`           | fleet |
| GET    | `/catalog/products`                     | catalog (resolved `EffectiveProduct` list — Load Builder) |
| GET    | `/catalog/dimensions`                   | catalog (override-management surface) |
| GET/PUT| `/catalog/dimensions/{productId}`       | catalog |
| POST   | `/load/optimize`                        | load |
| GET    | `/load/{id}`                            | load |
| POST   | `/routing/plan`                         | routing |
| GET    | `/routing/plan/{id}`                    | routing |
| POST   | `/routing/plan/{id}/approve`            | routing (write-back) |
| POST   | `/compliance/check-route`               | compliance |
| GET/POST | `/compliance/restricted-points`       | compliance |
| PUT    | `/compliance/restricted-points/{id}`    | compliance |
| POST   | `/workflow/plans`                       | workflow (step 1+2: ingest a date's orders + deep analysis) |
| GET    | `/workflow/plans/latest?date=`          | workflow |
| GET    | `/workflow/plans/{id}`                  | workflow |
| POST   | `/workflow/plans/{id}/assign`           | workflow (step 3: sweep-CVRP truck assignment + sequencing) |
| POST   | `/workflow/plans/{id}/pack`             | workflow (step 4: LIFO tier packing, all trucks) |
| PUT    | `/workflow/plans/{id}/loads/{vehicleId}/sequence` | workflow (manual stop reorder → re-pack) |
| POST   | `/workflow/plans/{id}/review`           | workflow (step 5: restricted-point check + AI reroute/load-adjust) |
| POST   | `/workflow/plans/{id}/push`             | workflow (step 6: routes + packing manifests → GableLBM) |
| POST   | `/auth/login`                           | auth (**public**: staff email → GableLBM `validate-staff` → AI_LM session JWT) |

Write routes are gated by `middleware.RequireRole("admin","owner","dispatcher","yard")`.
`/auth/login` is public (registered as a public path on the auth middleware) — it is
how a staff session is obtained. Session tokens are HMAC-signed with `SESSION_SECRET`.

## Digital-Twin Geometry Resolution

The Load Builder renders each product as a scaled box (a **digital twin**) against the
truck bed. GableLBM's PIM is the **canonical** source of per-product L/W/H; AI_LM keeps
optional overrides. The catalog service resolves the winning geometry per product:

```
OVERRIDE  (non-zero AI_LM product_dimensions row, default_source=MANUAL)
   └─▶ PIM     (non-null length/width/height from GableLBM /api/integration/products)
          └─▶ FALLBACK (no usable dims → has_geometry=false → UI flags it, no zero-box)
```

- **Resolver:** `catalog.Service.ListEffectiveProducts(ctx)` + the unit-testable
  `resolveGeometry(p gable.Product, ovr *Dimension)` helper. The merge is backend-owned —
  `*gable.Client` is injected into `catalog.Service` via the narrow `productSource`
  interface (wired in `cmd/server/main.go`). Weight = override else PIM weight.
- **Output:** `EffectiveProduct` (`catalog/model.go`) — `geometry_source` is
  `OVERRIDE`/`PIM`/`FALLBACK`, `has_geometry` is the render gate.
- **Endpoint:** `GET /api/v1/catalog/products` returns `[]EffectiveProduct`. Returns
  `502 Bad Gateway` when GableLBM is unreachable so the UI distinguishes outage from an
  empty catalog.
- **Shared scaling contract:** `_scale = 1/12` in `Load3DVisualizer.ts` —
  **1 inch = 1/12 Three.js world unit**. GableLBM's PIM preview
  (`<gable-product-twin-3d>`) uses the identical factor; a 96″ board is `8` world units in
  both apps. Do not change one side without the other.
- **Dependency:** hydrating the full catalog requires GableLBM's **unfiltered** bulk
  product pull (`GetProductsWithWeight` calls `/api/integration/products` with no params →
  `LIMIT 1000`). GableLBM commit `b5170de` enables this; an empty `?q=` does **not** work
  (it still counts as "no filter" but the older guard rejected it).

See `INTEGRATIONS.md` for the consumer contract and `ARCHITECTURE.md` for the module map.

## Key Conventions

(Carried over from GableLBM.)

### Database
- PKs are UUID v4 (`uuid_generate_v4()` via `uuid-ossp`).
- Physical quantities: `DECIMAL(19,4)`. Money in AI_LM-native tables: **BIGINT cents**.
- Weights/dimensions: `BIGINT` lb and integer inches where exact; lat/lng `DOUBLE PRECISION`.
- Migrations in `backend/migrations/` as numbered SQL with a sibling `_NNN_*_down.sql`
  rollback. The migrator (`cmd/migrate`) skips `*_down.sql`. Current set:
  `001_ai_lm_core`, `002_route_plan_loads`, `003_workflow_plans`.

### Backend Code
- Config: env vars with `godotenv` fallback (`internal/config/config.go`). Default DB URL
  points to **:5434** (docker-compose mapping). Integration: `GABLE_API_URL`,
  `GABLE_INTEGRATION_KEY`.
- Server entry point: `backend/cmd/server/main.go` — wires every module repo→service→handler.
- Error envelope (`pkg/httputil`): `{ "error": { "code", "message" }, "meta": { "request_id" } }`.

### Frontend Code
- Layout shell: `<ailm-app-shell>` (collapsible sidebar). Pages under `app/src/pages/`.
- **The primary surface is `/plan` (`<ailm-plan-workflow>`)** — a single-page
  five-step guided workflow (Ingest & Analyze → Assign Trucks → Pack Loads →
  Route Review → Manifest & Push) that replaced the old separate Dispatch Board
  and Load Builder pages (`/dispatch` and `/load` redirect to it). It includes
  per-truck 3D packing with wood-tone bundle twins and a packing-step playback
  slider, stop resequencing, the AI compliance-resolution log, and the final
  push to GableLBM. Orders come solely from real GableLBM data (date picker +
  ingest); there is no demo-seed scaffolding.
- `<ailm-load-3d-visualizer>` supports `colorMode="wood"` (realistic lumber
  twins, per-stop edge accents) and `visibleSteps` for pack-order playback.
- All custom elements use the `ailm-` prefix.
- Design tokens in `tailwind.config.js`; never hardcode colors. Use JetBrains Mono for all
  numbers/weights/dimensions.
- Adding a page: create the component under `app/src/pages/…`, register it in
  `app/src/routes.ts`, and map its tag in `app/src/app.ts` `_pathToTag`.

### Design System

| Token | Hex | Usage |
|-------|-----|-------|
| Gable Green | `#00FFA3` | Primary actions, success, active glow |
| Deep Space | `#0A0B10` | Global background |
| Slate Steel | `#161821` | Cards, sidebar, modals |
| Safety Red | `#F43F5E` | Errors, GVW fail |
| Amber Warn | `#FBBF24` | GVW warn, near-limit axles |
| Blueprint Blue | `#38BDF8` | Technical data, links |

**Body font:** Outfit | **Data font:** JetBrains Mono | **Theme:** Industrial Dark.

## Common Commands

### Backend (`cd backend`)
```bash
go run ./cmd/server                # run API (port 8090, needs DB on :5434)
go run ./cmd/migrate               # apply SQL migrations
go run ./cmd/seed                  # seed demo fleet profiles + restricted points
go build ./... && go vet ./... && go test ./...
make run | migrate | seed | build | vet | test | tidy
```

Override DB connection when Postgres is on the standard port:
```bash
DATABASE_URL="postgres://gable_user:gable_password@localhost:5432/ai_lm_db?sslmode=disable" go run ./cmd/server
```

### Frontend (`cd app`)
```bash
npm install
npm run dev          # Vite dev server on :5173 (proxies /api → :8090)
npm run build        # tsc -b && vite build
npx tsc --noEmit     # type-check only
```

### Infrastructure (root)
```bash
docker compose up -d # Postgres on :5434 (db ai_lm_db)
```

## Integration Contract (the licensing surface)

AI_LM consumes these GableLBM endpoints (all `X-Integration-Key` gated; base URL is
`GABLE_API_URL`, key is `GABLE_INTEGRATION_KEY`):

- `GET  /api/integration/products`               → per-unit `weight_lbs` **plus canonical
  3D geometry** (`length_in`, `width_in`, `height_in`, `stackable`, `geometry_source`).
  Called **with no params** by `gable.Client.GetProductsWithWeight` for a full bulk pull
  (`LIMIT 1000`); `?category=`/`?q=` narrow it to a `LIMIT 20` typeahead.
- `GET  /api/integration/vehicles`               → fleet (id, name, type, capacity, make/model/year).
- `GET  /api/integration/drivers`                → drivers for route write-back.
- `GET  /api/integration/orders?date=&status=CONFIRMED` → orders + line items
  (`product_id, sku, quantity, weight_lbs`), delivery `latitude/longitude` where present,
  and `branch_id` — the yard the order ships from (`NOT NULL` upstream).
  `date` filters on the order's `scheduled_delivery_date`.
- `GET  /api/integration/locations`              → the dealer's active branches (yards):
  `{id, name, address, latitude?, longitude?}`. Coordinates are **nullable** — a branch that
  has never been geocoded omits them, and must not be read as 0,0. `workflow.Ingest` matches
  them against the orders' `branch_id` to root a plan at the yard the load leaves from
  (`depot_source = BRANCH`) ahead of the install-wide `DEPOT_LAT`/`DEPOT_LNG`; a run spanning
  several branches falls back and records why in `depot_note` rather than picking one.
- `POST /api/integration/delivery-routes`        → write-back of an approved plan
  (`vehicle_id, driver_id, scheduled_date, stops[]{order_id, sequence, lat, lng}`,
  optional `load_manifest` JSON — the 3D packing manifest that powers GableLBM's
  yard **Pack Trucks** step-by-step loading surface).
  Idempotent on `(vehicle_id, scheduled_date)`.
- `POST /api/integration/validate-staff`         → staff login entitlement check.
  Request `{email}`; response `{staff_id, email, name, entitled, roles[], modules[]}`.
  Called by `gable.Client.ValidateStaff` from `POST /api/v1/auth/login`; when
  `entitled` is true AI_LM mints its own session JWT (it never stores credentials).

A different ERP can satisfy this contract to reuse AI_LM unchanged.

## Pre-Flight Checks (before declaring work done)
- `cd app && npx tsc --noEmit` (or `npm run build`)
- `cd backend && go build ./... && go vet ./... && go test ./...`
- New DB columns: UUID PKs, `DECIMAL(19,4)` for quantities, money-as-cents.
- UI uses design-system tokens (no hardcoded colors), JetBrains Mono for numbers.
- New endpoints under `/api/v1` and wired into a `RegisterRoutes` call in
  `backend/cmd/server/main.go`.

## Roadmap & Recommended Next Work

### Recently completed (do not re-recommend)
- **PIM-canonical geometry resolver** — `EffectiveProduct` + `resolveGeometry`
  (OVERRIDE → PIM → FALLBACK), `GET /api/v1/catalog/products`, `gable.Client` carrying PIM
  dims, and the Load Builder loading real products as scaled digital twins (commit
  `ee95607`). Depends on GableLBM's unfiltered bulk pull (`b5170de`).
- **End-to-end guided dispatch workflow** — the `internal/workflow` orchestrator
  + the single-page `/plan` stepper (merging the old Dispatch Board and Load
  Builder): date ingest with per-order deep analysis, sweep-CVRP truck
  assignment, LIFO **tier** packing (last stop bottom, first stop on top, every
  board an individual placement with a pack `step`), restricted-point review
  with automatic reroute/load-adjust, and manifest push to GableLBM (dispatch
  board + yard Pack Trucks). Items #3 and #5 of the list
  below are largely superseded by this (order-driven packing exists; stacking
  is handled by per-SKU banded bundles).

### Recommended next (grounded in current code)
1. **`is_override` clarity in `product_dimensions`.** Override-vs-default is currently
   inferred from non-zero dims + `default_source`. A first-class flag (or a clean
   `MANUAL`/`PIM_CACHE` enum) would make `resolveGeometry` unambiguous and testable, and
   stop a legitimately-zero dimension from reading as "no override".
2. **Persist/refresh a PIM geometry cache.** `ListEffectiveProducts` hits GableLBM live on
   every call. Cache the bulk pull (TTL or webhook-invalidated) so the Load Builder degrades
   gracefully when GableLBM is mid-deploy instead of returning `502`.
3. **Load straight from an order.** Today the user hand-adds products. Wire
   `/api/integration/orders` → Load Builder so a dispatcher optimizes an actual day's
   delivery, not a manual basket.
4. **Real distance-matrix routing provider.** Swap the nearest-neighbor + 2-opt heuristic
   behind `routing` for a Maps/OSRM matrix; the interface seam already exists.
5. **Stackability + orientation in the solver.** `stackable` is carried end-to-end but the
   placement solver does not yet forbid stacking on non-stackable items (e.g. the 6×6 PT
   post) or constrain long-piece orientation.

### Out of Scope (future phases)
- Mesh/GLTF product assets (parametric boxes only; `geometry_source` reserves the seam).
- ML-based placement (MVP solver is deterministic behind `load.Solver`).
- PostGIS geometry (MVP uses lat/lng + haversine buffer).
- Commercial multi-ERP adapters + licensing/metering (API shape is designed for it).
- Horizontal scaling (single instance; in-memory middleware).
