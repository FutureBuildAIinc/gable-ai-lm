<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# DEVOPS.md — Deployment Source of Truth

Operational source-of-truth for **AI_LM** deployments. Pairs with the specs in `.do/` and
with `INTEGRATIONS.md` (the upstream GableLBM contract). When the deploy topology changes,
update **this file first**.

## Platform

Digital Ocean App Platform (PaaS, Dockerfile-based, auto-deploy on push). Inspected and
managed via **`doctl`** (`~/.local/bin/doctl`, authenticated, default context) — not the
web console.

## What is actually deployed

| Spec | Branch | DO App | App ID | URL | Logical DB |
|---|---|---|---|---|---|
| `.do/app-staging.yaml` | `main` | **`ai-lm-staging`** | `8a274c57-dee2-4053-ac3c-40fe2528ca9e` | **https://load.gablelbm.com** (+ https://ai-lm-staging-b6ssv.ondigitalocean.app) | `ai_lm_staging` |
| `.do/app-demo.yaml` | `community` | *(not created)* | — | (intended `demo.ai-lm.gable.com`) | `ai_lm_demo` |

> **Custom domain:** `load.gablelbm.com` is declared in `app-staging.yaml`
> (`domains:` block). gablelbm.com's DNS zone is **not** in this DO account, so the
> DNS provider needs `CNAME load.gablelbm.com → ai-lm-staging-b6ssv.ondigitalocean.app`;
> App Platform provisions the TLS cert once the record resolves. Check status with
> `doctl apps get 8a274c57-dee2-4053-ac3c-40fe2528ca9e --output json | jq '.[0].domains'`.
>
> **Functional dependency:** the guided workflow needs GableLBM `community` to carry
> the AI_LM dispatch support (migration 075, scheduled-date orders, demo seed,
> manifest storage, yard Pack Trucks) — see
> https://github.com/futurebuildai/GableLBM-main/pull/16. Until that merges, ingest
> matches orders by creation date only, the demo-seed button 404s, and pushed routes
> carry no packing manifest.
>
> **Known issue (2026-06-12):** ingest against demo.gablelbm.com returns
> `401 invalid integration key` — `GABLE_INTEGRATION_KEY` (ai-lm-staging) does not
> match `INTEGRATION_API_KEY` (gablelbm-demo). Both are encrypted DO secrets, so the
> fix needs someone holding the value: either paste gablelbm-demo's key into
> ai-lm-staging, or rotate BOTH to a fresh shared value (coordinate with FB Brain,
> which uses the same GableLBM key).

> **Important reality check:** `ai-lm-staging` (tracking **`main`**) is the **only live
> AI_LM environment**. `app-demo.yaml` targets a `community` branch and a `demo.ai-lm.gable.com`
> domain, but **AI_LM has no `community` branch and no `ai-lm-demo` app exists in DO**. Treat
> `app-demo.yaml` as a not-yet-provisioned template. Verify with
> `doctl apps list --format ID,Spec.Name,DefaultIngress` — only `ai-lm-staging` appears.

## Integration target

`ai-lm-staging` is wired to GableLBM's **`community`** demo:
`GABLE_API_URL=https://demo.gablelbm.com` (public, in the spec) and `GABLE_INTEGRATION_KEY`
(encrypted secret, must equal GableLBM's `INTEGRATION_API_KEY`). The Load Builder's catalog
is hydrated from that GableLBM via the unfiltered bulk product pull — see `INTEGRATIONS.md`.

## Deploy anatomy

```
git push origin main ──▶ DO App Platform pulls main
                         ├─ build backend/Dockerfile → main + migrate + seed (port 8090)
                         ├─ build app/Dockerfile      → nginx + Vite SPA (port 8080)
                         ├─ deploy backend + frontend
                         └─ POST_DEPLOY job: ./migrate, then ./seed ONLY if DEMO_SEED=true
```

- App Platform path-routes `/api`, `/healthz`, `/metrics` to the backend (with
  `preserve_path_prefix: true` — without it the Go router 404s on `/healthz/live`); `/` to
  the frontend. `CORS_ORIGINS` is intentionally unset on staging (same-origin path routing).
- `INSTANCE_COUNT` must stay **1** (in-memory middleware/state).
- The post-deploy job sets `AUTH_MODE=dev` on itself so migrate/seed don't trip the
  fail-closed config path (the job serves no auth'd HTTP). Note the fail-closed requirement
  is **`SESSION_SECRET`**, not `JWKS_URL` — AI_LM mints its own staff session tokens and
  `JWKS_URL` is optional. Earlier revisions of this file said otherwise; they were wrong.
- **The demo seed is now opt-in.** The job runs `./migrate` unconditionally and `./seed`
  only when `DEMO_SEED=true`. Both current specs set it, because both currently hold
  disposable demo data. Any environment carrying real dealer data must not declare it —
  full contract and pre-production checklist in [`.do/README.md`](.do/README.md).
- Healthy deploy = Phase `ACTIVE`, all steps green (e.g. `13/13`).

> ⚠️ Both specs run with **`AUTH_MODE=dev`**, and both map public hostnames. That means an
> unauthenticated caller can reach the route write-back. It is acceptable only because
> these environments hold demo data. Read `SECURITY.md` and `.do/README.md` before pointing
> either one at a real customer.

## Runbook (`doctl`)

```bash
# Confirm only ai-lm-staging is live
doctl apps list --format ID,Spec.Name,DefaultIngress

# Watch the newest deployment
doctl apps list-deployments 8a274c57-dee2-4053-ac3c-40fe2528ca9e \
  --format ID,Cause,Phase,Progress,Created | head

# Logs: service, build, and the post-deploy migrate/seed job
doctl apps logs 8a274c57-dee2-4053-ac3c-40fe2528ca9e backend          --type run
doctl apps logs 8a274c57-dee2-4053-ac3c-40fe2528ca9e backend          --type build
doctl apps logs 8a274c57-dee2-4053-ac3c-40fe2528ca9e migrate-and-seed --type run

# Force a redeploy (e.g. re-run seed); push a changed spec
doctl apps create-deployment 8a274c57-dee2-4053-ac3c-40fe2528ca9e --force-rebuild
doctl apps update            8a274c57-dee2-4053-ac3c-40fe2528ca9e --spec .do/app-staging.yaml
```

### Deploy + verify

```bash
git push origin main
doctl apps list-deployments 8a274c57-dee2-4053-ac3c-40fe2528ca9e \
  --format Cause,Phase,Progress,Created | head -3      # wait for ACTIVE

BASE=https://ai-lm-staging-b6ssv.ondigitalocean.app
curl -6 --retry 4 --retry-all-errors -sf "$BASE/healthz/live" && echo OK
curl -s "$BASE/api/v1/catalog/products" | jq 'length, (map(.geometry_source) | group_by(.) | map({(.[0]): length}))'
# expect >0 products; a few PIM/has_geometry=true, the rest FALLBACK
```

The host's IPv6 path can be flaky — `curl -6 --retry 4 --retry-all-errors` is the reliable
incantation.

## Secrets

`DATABASE_URL` (DO binding `${ai-lm-pg-staging.DATABASE_URL}`) and `GABLE_INTEGRATION_KEY`
are the only secrets, both encrypted env vars; never inline them in YAML. `GABLE_API_URL`
is a public base URL (not secret).

`GABLE_INTEGRATION_KEY` is a **write** credential — the route write-back uses it. Treat a
leak as equivalent to leaking write access to the ERP's dispatch board.

`AUTH_MODE=dev` ⇒ no session/JWT key needed on staging. For a real auth path you must
**remove `AUTH_MODE`** and add **`SESSION_SECRET`** (`openssl rand -hex 32`) plus
`CORS_ORIGINS`; the server is fail-closed and will refuse to start without them.
`JWKS_URL` is optional and only widens verification to externally-issued tokens. The full
checklist — including the staff-login prerequisite on the GableLBM side — is in
[`.do/README.md`](.do/README.md).

## Rollback

```bash
doctl apps list-deployments 8a274c57-dee2-4053-ac3c-40fe2528ca9e
doctl apps create-deployment 8a274c57-dee2-4053-ac3c-40fe2528ca9e --restore-deployment <deployment-id>
```

The migrate/seed job re-runs. DO rollback does not undo schema changes — fix a bad
migration with a corrective forward migration.

> **Caution:** the seed is *not* fully idempotent yet. Vehicle profiles upsert, but
> restricted points are inserted unconditionally, so a rollback or forced redeploy with
> `DEMO_SEED=true` adds duplicate restricted points rather than converging. Until that is
> fixed, remove `DEMO_SEED` before a rollback unless you specifically want the demo data
> reset.

## Deploys are not CI, and CI is not a deploy

Deployment is DO `deploy_on_push`. `gh run list` won't show deploy status; use
`doctl apps list-deployments`.

Since the migration into the Gable ecosystem there **is** a GitHub Actions workflow —
`.github/workflows/ci.yml` — but it only gates pull requests (build, vet, gofmt, race
tests, coverage reporting, frontend type-check/test/build, and the REUSE licensing gate,
plus an advisory nightly `govulncheck`). It does not deploy anything, and DigitalOcean does
not wait for it. A push to `main` deploys whether or not CI is green, so protect `main`
with the aggregated **`CI`** status check.

## Repository move

AI_LM migrated from `futurebuildai/AI_LM` to
[`FutureBuildAIinc/gable-ai-lm`](https://github.com/FutureBuildAIinc/gable-ai-lm). The
specs in `.do/` now name the new repository. Before applying them, the DigitalOcean GitHub
App must be granted access to it — App Platform cannot repoint at a repo it has no grant
for. Push, grant, then `doctl apps update`, in that order.
