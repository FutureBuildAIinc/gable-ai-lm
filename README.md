<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# AI_LM — AI Load Management & Dispatch Optimization

**AI_LM turns a dispatcher's morning whiteboard into a solved plan.** It takes a
day's confirmed orders out of a lumber & building-materials (LBM) ERP, decides
which truck carries what, packs each truck in three dimensions in the order the
stops will be unloaded, checks the resulting axle weights and route against
legal limits, and hands the dispatcher a plan that is 80–90% finished before
anyone touches it.

It is a **satellite** of [Gable](https://github.com/FutureBuildAIinc/gable), the
open commons for LBM operations: its own Go service, its own PostgreSQL
database, its own Lit UI, talking to the ERP over a narrow integration
contract. Gable is the system of record. AI_LM is the thing that decides how
the truck gets loaded.

> **Maturity: pre-GA.** This is a capable, working system that is not yet
> production-hardened. Read [Where AI_LM actually is](#where-ai_lm-actually-is)
> before you deploy it for a real customer — that section is deliberately blunt.

## What it does

**The guided dispatch workflow.** One screen, five steps, each one persisting
its artifacts so a run can be resumed or replayed:

| Step | What happens |
|---|---|
| **1. Ingest & Analyze** | Pull a calendar date's confirmed orders from the ERP and resolve every line to real geometry — length, width, height, weight, stackability, volume, and a shape profile (long-load / compact / mixed). |
| **2. Assign Trucks** | Split the orders across the fleet as a capacity-constrained vehicle routing problem, and sequence each truck's stops. |
| **3. Pack Loads** | 3D-pack each truck **LIFO by stop** — the last delivery loads first — as realistic banded bundles rather than abstract boxes. Routes may be re-sequenced if packing demands it. |
| **4. Route Review** | Check every route against restricted points (bridge weight limits, overpass clearances) and auto-resolve flags by rerouting or re-balancing across trucks. |
| **5. Manifest & Push** | Write the final routes and packing manifests back to the ERP's dispatch board. |

**3D load solving.** A deterministic shelf-packing solver places each item in
inches from the front-left-floor corner of the bed, respecting stackability and
a usable-volume budget. The frontend renders the result with Three.js at
**1 inch = 1/12 world unit** — the same render contract as GableLBM's product
twin, so a board is the same size in both applications.

**Axle and GVW compliance.** Vehicle profiles carry bed dimensions, GVWR, tare
weight, and per-axle positions and ratings. Every solved load reports per-axle
loading and gross vehicle weight with a pass / warn / fail verdict, so the
overload is caught in the yard rather than at a scale.

**DOT securement.** Tie-down counts and placement are computed from a
jurisdiction ruleset (`US_FMCSA` by default, `CA_NSC` also modelled) and the
bed's actual tie-down anchor spacing, so the strap plan lands on real anchors.

**Route optimization.** Nearest-neighbour construction plus 2-opt improvement,
over a real road distance and duration matrix from
[OpenRouteService](https://openrouteservice.org) (`driving-hgv` — heavy trucks,
not cars) when `ORS_API_KEY` is set. Without a key it falls back to a haversine
heuristic and keeps running rather than hard-failing.

**Optional AI briefing.** With `OPENROUTER_API_KEY` set, a dispatch briefing is
generated through an OpenAI-compatible client against an open-weight model.
Unset, the endpoint reports "unavailable" and nothing in the workflow blocks.

## How it relates to Gable

AI_LM owns no business data of record. It is a **consumer** of the GableLBM
integration API and a writer back to it:

```
GableLBM (system of record)                    AI_LM (this repo)
  GET  /api/integration/products    ────▶  catalog     dimensions, weights, stackability
  GET  /api/integration/vehicles    ────▶  fleet       axle & bed profiles keyed by ERP id
  GET  /api/integration/drivers     ────▶  routing     driver assignment
  GET  /api/integration/orders      ────▶  workflow    orders, lines, delivery geo
  POST /api/integration/validate-staff ─▶  auth        staff entitlement check
  POST /api/integration/delivery-routes ◀─  workflow   approved routes + manifests
```

Every call carries `X-Integration-Key`. The ERP schema is never touched:
supplementary data the ERP does not model — axle profiles, bed geometry,
per-product dimension overrides, restricted points — lives in AI_LM's own
database, keyed by the ERP's UUIDs.

The full consumer-side contract is in [INTEGRATIONS.md](./INTEGRATIONS.md); the
system shape is in [ARCHITECTURE.md](./ARCHITECTURE.md).

### Ecosystem

| Repo | Role |
|---|---|
| [`gable`](https://github.com/FutureBuildAIinc/gable) | The ERP host. System of record; serves the integration API AI_LM consumes. |
| **`gable-ai-lm`** *(this repo)* | Load management and dispatch optimization satellite. |
| [`gable-sdk`](https://github.com/FutureBuildAIinc/gable-sdk) | Zero-dependency Go connector SDK for the host's installable-app seam. |
| [`openlbm`](https://github.com/FutureBuildAIinc/openlbm) | The OpenLBM Standard: canonical license texts, the CLA, and the trademark policy. |

## Quick start

**Prerequisites:** Go 1.25+, Node 22 (or 20.19+), Docker.

```bash
# 1. Postgres on :5434 (database ai_lm_db)
docker compose up -d

# 2. Backend on :8090
cd backend
cp .env.example .env       # AUTH_MODE=dev is preset for local work
go run ./cmd/migrate       # apply backend/migrations/*.sql
go run ./cmd/seed          # optional: demo fleet profiles + restricted points
go run ./cmd/server

# 3. Frontend on :5173 (Vite proxies /api → :8090)
cd ../app
npm install
npm run dev
```

Open <http://localhost:5173>. Health at `/healthz/live` and `/healthz/ready`,
Prometheus metrics at `/metrics`.

To drive it with real data, point `GABLE_API_URL` at a running GableLBM
instance (default `http://localhost:8080`) and set `GABLE_INTEGRATION_KEY` to
that instance's `INTEGRATION_API_KEY`. Without them the catalog and order pulls
return `502` — which is deliberate, so an outage is distinguishable from an
empty catalog.

`backend/Makefile` wraps the common commands: `make run`, `make migrate`,
`make seed`, `make build`, `make vet`, `make test`, `make tidy`.

## Layout

```
backend/     Go service
  cmd/         server, migrate, seed
  internal/    ai, auth, catalog, compliance, config, fleet, gable,
               load, routing, workflow
  pkg/         database, httputil, metrics, middleware
  migrations/  NNN_name.sql (+ NNN_name_down.sql)
app/         Lit 3 + TypeScript + Vite + Tailwind
  src/pages/     PlanWorkflow, FleetProfiles, CompliancePoints, Login
  src/components/ Load3DVisualizer (Three.js), RouteMap (Leaflet)
  src/services/  aiLmService (typed API client)
.do/         DigitalOcean App Platform manifests  — READ .do/README.md FIRST
.github/     CI, issue and PR templates, the REUSE licensing gate
```

## Where AI_LM actually is

The features above are real and they work. These limitations are also real, and
you should know them before choosing where to run this.

**It is single-tenant.** There is no tenant column, no tenant scoping in the
query layer, and no per-tenant configuration. One deployment serves one dealer.
Do not put two dealers' data in one instance.

**ERP portability is a design goal, not a delivered fact.** The intent is that
any ERP satisfying the integration contract can host AI_LM unchanged, and the
contract itself is genuinely narrow — six routes. But the seam is not closed
yet: GableLBM wire types from `internal/gable` still cross into downstream
domain code, appearing in roughly ten non-test files across `routing`,
`workflow`, `catalog`, and `auth`. Swapping the ERP today means editing those
modules, not just the client. Tightening that boundary is roadmapped.

**The axle and GVW math is not certified.** It is a well-tested engineering
heuristic built from vehicle profiles you supply. It is not a compliance
product, it has not been audited against a jurisdiction's enforcement
standards, and nothing here relieves the carrier of responsibility for the
weight on their axles. Treat every verdict as a strong prompt to check, not as
a legal clearance. If you find a load that AI_LM passes and a scale does not,
report it privately — see [SECURITY.md](./SECURITY.md).

**The reference deployments run with authentication disabled.** Both manifests
in [`.do/`](.do/) set `AUTH_MODE=dev`, and one of them maps a public hostname.
That is a known gap, documented in [`.do/README.md`](.do/README.md), and it is
the single thing that must change before any deployment holding a real
customer's data. AI_LM's API is not read-only — it pushes routes into the ERP.

**Test coverage is partial and uneven.** Around 38% of statements at the time of
writing, and rising. The load solver, securement rules, workflow logic, config,
and the OpenRouteService client are genuinely covered (43–75%); `compliance`,
`fleet`, the GableLBM client, and every package under `pkg/` sit at zero. CI
reports the live per-package figure on every run and deliberately does not gate
on it — see [CONTRIBUTING.md](./CONTRIBUTING.md).

**Horizontal scaling is unvalidated.** `INSTANCE_COUNT` must stay 1; parts of
the middleware hold in-process state.

[ROADMAP.md](./ROADMAP.md) tracks what is being done about all of this.

## Licensing

AI_LM is **source-available** under the
**OpenLBM Community Source License 1.0** — one license for the whole product,
not the multi-license split the host repo uses.

- **Free for Community Members, in production, immediately.** A Community
  Member is an organisation operating fewer than **50 locations** that is not
  controlled by (and does not control) an entity above **$1B** in annual
  revenue. Most independent LBM dealers qualify. No fee, no waiting period.
- **Everyone else needs a commercial license for production use.** Large
  chains, and anyone offering AI_LM as a hosted or managed service to third
  parties, are outside the carve-out. Reading, modifying, evaluating, and
  redistributing the source remain open to everyone.
- **It converts to AGPL-3.0-only five years** after each version is released.

Documentation and repository plumbing are under the OpenLBM Docs license. The
full per-path mapping is in [LICENSE-MAP.md](./LICENSE-MAP.md), enforced on
every pull request by the REUSE gate in CI.

The license texts under [`LICENSES/`](./LICENSES/) are **drafts pending
counsel**. The canonical Standard lives at
<https://github.com/FutureBuildAIinc/openlbm>; where they disagree, the
published Standard governs. The license grants no rights in the names or logos
"Gable", "AI_LM", "OpenLBM", or "FutureBuild" — a fork must be renamed.

## Contributing

- [CONTRIBUTING.md](./CONTRIBUTING.md) — build, gates, branch model, CLA
- [CONTRIBUTING-WITH-CLAUDE.md](./CONTRIBUTING-WITH-CLAUDE.md) — the same
  workflow when you are pairing with Claude Code
- [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md) — Contributor Covenant 2.1
- [SECURITY.md](./SECURITY.md) — **private** disclosure; never a public issue
- [ROADMAP.md](./ROADMAP.md) — what is planned, and what is deliberately not

Deeper reference: [ARCHITECTURE.md](./ARCHITECTURE.md) (system shape),
[INTEGRATIONS.md](./INTEGRATIONS.md) (the ERP contract), [DEVOPS.md](./DEVOPS.md)
(deploy runbook), [CLAUDE.md](./CLAUDE.md) (working conventions).
