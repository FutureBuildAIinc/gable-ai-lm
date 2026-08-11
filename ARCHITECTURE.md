<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# Architecture — AI_LM

AI_LM (AI Load Management & Compliance) is a **standalone microservice** for GableLBM that
balances truck loads across axles (avoiding GVW/axle fines) and pre-optimizes daily
delivery routing. It is deliberately decoupled from the ERP so it can later be licensed to
other ERPs. This document covers the system shape; `README.md` is the front door,
`CLAUDE.md` the working guide, `INTEGRATIONS.md` the consumer contract, `DEVOPS.md` the
deploy runbook, and `ROADMAP.md` the list of known gaps.

## 1. Principles

- **Standalone, not embedded.** Own Go binary, own PostgreSQL DB, own Lit UI. No shared
  process or schema with GableLBM — it reads/writes only over `/api/integration/*`.
- **Portable by design — not yet portable in fact.** Supplementary attributes GableLBM
  doesn't model (axle/bed profiles, per-product overrides) live in AI_LM, keyed by GableLBM
  UUIDs, and the integration contract is deliberately narrow. The *goal* is that any ERP
  satisfying that contract can host AI_LM unchanged. The **current reality** is that
  `internal/gable` wire types still cross into downstream domain code — roughly 60
  references across ~10 non-test files in `routing`, `workflow`, `catalog`, and `auth` —
  so swapping the ERP today means editing those modules, not just the client. Closing that
  seam is roadmapped; until it closes, treat portability as an architectural intent rather
  than a delivered property, and do not widen the leak.
- **Heuristics behind interfaces.** The load solver and route optimizer are deterministic
  heuristics implementing `load.Solver` / routing interfaces, so an AI/optimizer can
  replace them without touching callers.
- **Backend-owned merges.** Cross-source resolution (PIM geometry vs. local overrides) is
  computed in the catalog service, not the UI, so every client sees one consistent answer.

## 2. Topology

```
GableLBM (source of truth)                AI_LM (this service)
  GET  /api/integration/products  ─────▶  internal/gable     X-Integration-Key client
  GET  /api/integration/vehicles  ─────▶  internal/fleet      axle/bed profiles by gable id
  GET  /api/integration/drivers   ─────▶  internal/routing    driver assignment
  GET  /api/integration/orders    ─────▶  internal/catalog    product dims; weight from LBM
  POST /api/integration/          ─────▶  internal/auth      staff entitlement check
       validate-staff                     internal/load       3D placement + axle/GVW solver
  POST /api/integration/             ◀──  internal/workflow   guided 5-step dispatch run
       delivery-routes (write-back)       internal/compliance GVW rules + restricted points
```

- **Pattern:** modular monolith — one Go binary, modules under `backend/internal/`.
- **Module shape:** `model.go`, `repository.go` (pgx), `service.go`, `handler.go`; wired in
  `backend/cmd/server/main.go`.
- **Ports:** API on **8090**; Postgres on **5434** (docker-compose mapping).
- **API:** REST JSON at `/api/v1/*`; public `/health`, `/healthz/{live,ready}`, `/metrics`.

## 3. Modules

| Module | Responsibility |
|---|---|
| `gable` | Integration client to GableLBM (`X-Integration-Key`); wire types for products (incl. geometry), vehicles, drivers, orders, route write-back. |
| `fleet` | Vehicle profiles — axle configuration and bed dimensions keyed by GableLBM vehicle id. |
| `catalog` | Per-product override store **and** the PIM-geometry resolver (`EffectiveProduct`). |
| `load` | 3D placement solver + per-axle / GVW computation (`load.Solver`), usable-volume budget, LIFO-by-stop sequencing, and DOT securement (`US_FMCSA` / `CA_NSC` rulesets, anchor-spacing aware). |
| `routing` | Daily-order optimizer — nearest-neighbor + 2-opt, multi-load CVRP split by capacity. Uses an OpenRouteService `driving-hgv` distance/duration matrix when `ORS_API_KEY` is set, and a haversine heuristic when it is not. |
| `compliance` | GVW rule enforcement + restricted-point (bridge/overpass) flagging via haversine buffer. |
| `workflow` | The guided 5-step dispatch run (Ingest & Analyze → Assign → Pack → Route Review → Manifest & Push). One `Plan` row carries a whole run; each step persists its artifacts so the UI can resume or replay any stage. Also owns run locking, late-adds, priority overrides, and proof/sign-off. |
| `auth` | Staff login. Delegates the entitlement decision to GableLBM's `validate-staff`, then mints AI_LM's own HMAC session JWT signed with `SESSION_SECRET`. AI_LM never stores credentials. |
| `ai` | Optional OpenRouter client (OpenAI-compatible) behind the dispatch briefing. Degrades to "unavailable" with no key; never blocks the workflow. |

## 4. The Digital-Twin Pipeline

The crux feature. GableLBM's PIM is canonical for per-product L/W/H; AI_LM renders each
product as a scaled box against the truck bed.

```
PIM dims (GableLBM)            AI_LM overrides            EffectiveProduct
  length/width/height   ┐      product_dimensions   ┐      geometry_source: OVERRIDE|PIM|FALLBACK
  stackable, weight     ├────▶ (non-zero = override)├────▶ has_geometry: bool
  geometry_source       ┘      default_source       ┘      → GET /api/v1/catalog/products
                                                            → PlanWorkflow → Load3DVisualizer
```

- **Resolution priority:** OVERRIDE → PIM → FALLBACK (`catalog.Service.resolveGeometry`).
  FALLBACK sets `has_geometry=false` so the UI flags the SKU instead of rendering a
  zero-size box.
- **Dependency injection:** `*gable.Client` satisfies the `productSource` interface and is
  injected into `catalog.Service` (so the resolver is unit-testable with a fake source and
  degrades to overrides-only when nil).
- **Render contract:** `Load3DVisualizer.ts` `_scale = 1/12` — **1 inch = 1/12 Three.js
  world unit**, identical to GableLBM's `<gable-product-twin-3d>`. Solver coordinates are
  inches from the front-left-floor corner; the frontend maps solver `(x,y,z)` → three
  `(x,z,y)` (Y-up) and multiplies by `_scale`.
- **Failure mode:** `GET /api/v1/catalog/products` returns `502` when GableLBM is
  unreachable, distinguishing an outage from an empty catalog.

## 5. Frontend

- **Lit 3** Light-DOM web components, all `ailm-` prefixed; Tailwind "Industrial Dark".
- **3D:** `<ailm-load-3d-visualizer>` (Three.js). **Maps:** `<ailm-route-map>` (Leaflet).
  **Charts:** Chart.js.
- **Pages** (`app/src/routes.ts`): `/plan` (`<ailm-plan-workflow>` — the guided 5-step
  run, and the app's centre of gravity), `/fleet` (`<ailm-fleet-profiles>`), `/compliance`
  (`<ailm-compliance-points>`), `/login` (`<ailm-login>`, no app shell). `/`, `/dispatch`
  and `/load` all redirect to `/plan`: the former separate Dispatch Board and Load Builder
  pages were merged into it, so any doc still describing a `YardLoadView` page is
  describing something that no longer exists.
- **HTTP:** `services/aiLmService.ts` (typed) over `services/fetchClient.ts`; pages never
  call `fetch` directly. Relative `/api/v1/*` — Vite proxies to `:8090` in dev, nginx in
  prod.

## 6. Data & Auth

- PostgreSQL 16 via pgx v5. UUID PKs; `DECIMAL(19,4)` quantities; money as BIGINT cents in
  native tables; weights/dims as integers where exact; lat/lng `DOUBLE PRECISION`.
- Migrations in `backend/migrations/` (forward `NNN_*.sql` + sibling `_down.sql` the
  migrator skips). Applied so far: `001_ai_lm_core`, `002_route_plan_loads`,
  `003_workflow_plans`.
- **Single-tenant.** There is no tenant column and no tenant scoping in the query layer.
  One deployment serves one dealer.
- **Auth is `SESSION_SECRET`-first.** `internal/auth` delegates the entitlement decision to
  GableLBM's `validate-staff` and then mints AI_LM's **own** HMAC-signed session JWT;
  `pkg/middleware` verifies it. `JWKS_URL` is *optional* and only widens verification to
  externally-issued tokens — it is not the primary verifier and is not required. Anything
  in this repo's older notes describing JWKS as mandatory is out of date.
- `AUTH_MODE=dev` bypasses inbound auth entirely and is only safe on a laptop — see
  `SECURITY.md`. Outbound integration calls always carry `X-Integration-Key`.

## 7. Deployment

Digital Ocean App Platform, auto-deploy on push. Live env is **`ai-lm-staging`** (tracks
`main`), pointed at GableLBM `community` (`https://demo.gablelbm.com`). Full topology, app
IDs, and the `doctl` runbook are in `DEVOPS.md`; the pre-production checklist and the
`DEMO_SEED` contract are in [`.do/README.md`](.do/README.md).

CI is separate from deployment: `.github/workflows/ci.yml` gates pull requests
(build, vet, gofmt, race tests, type-check, frontend build, REUSE licensing), while
DigitalOcean's `deploy_on_push` does the deploying. A green CI run is not a deploy, and a
successful deploy is not evidence CI passed.
