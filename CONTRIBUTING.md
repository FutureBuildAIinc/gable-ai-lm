<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# Contributing to AI_LM

Thanks for your interest in AI_LM, the load-management and dispatch-optimization
satellite of the [Gable](https://github.com/FutureBuildAIinc/gable) ecosystem.
This guide covers how to build and run the project, the gates your change must
pass, and how inbound contributions are licensed.

Please also read the [Code of Conduct](./CODE_OF_CONDUCT.md). For security
issues, do **not** open a public issue — follow [SECURITY.md](./SECURITY.md).

If you work with Claude Code, there is a repo-specific companion guide:
[CONTRIBUTING-WITH-CLAUDE.md](./CONTRIBUTING-WITH-CLAUDE.md). It supplements
this document rather than replacing it — the gates below apply either way.

## Build & run

**Prerequisites:** Go **1.25+**, Node **22** (or 20.19+; Vite 7 requires one of
those), Docker for PostgreSQL **16**.

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

Open <http://localhost:5173>.

`backend/Makefile` wraps the common commands — `make run`, `make migrate`,
`make seed`, `make build`, `make vet`, `make test`, `make tidy`. They are thin
aliases for the `go` commands above; there is no root-level Makefile.

To point at a real GableLBM instance, set `GABLE_API_URL` (default
`http://localhost:8080`) and `GABLE_INTEGRATION_KEY` to that instance's
`INTEGRATION_API_KEY` in `backend/.env`. Without them, catalog and order pulls
return `502` — see [INTEGRATIONS.md](./INTEGRATIONS.md).

`AUTH_MODE=dev` disables authentication completely. It is fine on your laptop
and **never** anywhere reachable. See [SECURITY.md](./SECURITY.md).

For stack details, conventions, and gotchas, see [CLAUDE.md](./CLAUDE.md),
[ARCHITECTURE.md](./ARCHITECTURE.md), and [INTEGRATIONS.md](./INTEGRATIONS.md).

## Branch model

`main` is the only branch today. **Open your pull request against `main`.**

If `staging` and `community` branches appear later (the CI workflow and the
`.do/` manifests already anticipate them), this section will change; until
then, ignore any doc that tells you to target `staging`.

## Pull request workflow

1. Fork the repository, or branch directly if you have write access.
2. Branch off `main`: `git checkout -b feat/short-description main`.
3. Make your change. Keep commits focused — one logical change each.
4. Run the pre-flight gates below **before pushing**.
5. Open the PR against `main` and fill out the template.
6. Address review feedback; a maintainer merges once it is approved and green.

### Pre-flight checklist

These are **exactly** the gates CI enforces — see
[`.github/workflows/ci.yml`](./.github/workflows/ci.yml). Running them locally
first saves a round trip.

```bash
# ---- Backend (the `backend` job) -----------------------------------------
cd backend
gofmt -l .            # must print nothing; `gofmt -w .` fixes it
go vet ./...
go build ./...
go test -race ./...   # no database required — see below

# ---- Frontend (the `frontend` job) ---------------------------------------
cd app
npm ci
npx tsc --noEmit
npm run lint --if-present
npm run test
npm run build

# ---- Licensing (the `license` job) ---------------------------------------
cd <repo root>
pipx install reuse==6.2.0          # once
reuse lint
python3 .github/scripts/reuse_gate.py
```

**The backend suite needs no database.** Every test runs against fakes and
in-memory fixtures, so `go test ./...` passes on a machine with Postgres
stopped. That is why CI has no `services: postgres` block. If you add a test
that genuinely needs a live database, say so in the PR — the service block goes
into `ci.yml` at the same time, it does not get skipped.

**Run `reuse lint` on a clean tree.** If `app/node_modules/` exists and your
working copy has uncommitted top-level directories, older versions of `git`
can fail to report the ignore, and `reuse` will try to scan every dependency.
On a fresh checkout — which is what CI has — it scans the repository only.

**Not a gate: `govulncheck`.** It runs in its own advisory CI job on every push
and nightly. It reports known vulnerabilities in Go dependencies but does not
block your PR — a CVE disclosed overnight is not your bug to fix. Maintainers
triage those findings.

**Also not a gate: code coverage.** CI measures it, uploads it as an artifact,
and prints a per-package summary — but there is no minimum and no failure for
lowering it. The backend baseline is around 38% of statements and very uneven:
`internal/load` and `internal/ai` are well covered, while `internal/compliance`,
`internal/fleet`, `internal/gable`, and everything under `pkg/` are at zero.
Those zeros are the useful place to start. Just don't expect a number to police
it.

## Conventions

- **Module shape.** Each domain module under `backend/internal/` is
  `model.go` / `repository.go` (pgx) / `service.go` / `handler.go`, wired in
  `backend/cmd/server/main.go`. Follow the shape of a neighbouring module.
- **Migrations** live in `backend/migrations/` as `NNN_name.sql` with a sibling
  `NNN_name_down.sql` that the migrator skips. They are append-only: fix a bad
  migration with a corrective forward migration, never by editing history.
- **Frontend.** Lit 3 Light-DOM components, all `ailm-` prefixed. Pages never
  call `fetch` directly — go through `app/src/services/aiLmService.ts`.
- **Units.** Weights in pounds, dimensions in inches, coordinates in decimal
  degrees. The 3D render contract is **1 inch = 1/12 Three.js world unit**,
  matching GableLBM's product twin. Do not introduce a second scale.
- **SPDX headers.** Every file carries one. Copy the header from a neighbouring
  file in the same directory; the correct identifier for each path is in
  [LICENSE-MAP.md](./LICENSE-MAP.md). CSS uses `/* ... */`, not `//`.
- **No secrets, no live hostnames, no customer data** in commits, tests, or
  fixtures.

## Licensing of contributions (inbound = OpenLBM, via CLA)

AI_LM is licensed **per component** — see [LICENSE-MAP.md](./LICENSE-MAP.md),
the SPDX header on each file, and [REUSE.toml](./REUSE.toml). In practice the
map is short: everything under `backend/` and `app/`, plus the deployment
manifests, is `LicenseRef-OpenLBM-Community-Source-1.0`; documentation and repo
plumbing is `LicenseRef-OpenLBM-Docs-1.0`.

**Inbound contributions are licensed under the same license that governs the
file(s) you change.**

Because these are custom source-available licenses rather than an off-the-shelf
inbound=outbound OSS license, we use a **Contributor License Agreement (CLA)**.
Before your first contribution is merged you will be asked to agree to it. The
canonical OpenLBM Standard texts and the CLA live in the OpenLBM repository:

> **OpenLBM Standard & CLA:** <https://github.com/FutureBuildAIinc/openlbm>

The texts under [`LICENSES/`](./LICENSES/) are drafts pending counsel review;
where they and the published Standard disagree, the published Standard governs.

By submitting a pull request, you confirm that:

- The contribution is your original work, or you have the right to submit it.
- You agree to license it under the applicable component license, per the CLA.
- You are not knowingly including third-party code under incompatible terms.

## Reporting bugs & requesting features

Use the issue templates. Please search existing issues first.

- **Bug reports** — include your commit SHA, environment, and a reproduction.
- **Feature requests** — describe the LBM dispatch or yard problem you are
  solving, not just the mechanism.
- **Compliance-math bugs** — if the solver reports a load as compliant when it
  is not, that is a safety issue. Report it privately per
  [SECURITY.md](./SECURITY.md), not in a public issue.

[ROADMAP.md](./ROADMAP.md) lists what is already planned; checking it first
avoids duplicate proposals.
