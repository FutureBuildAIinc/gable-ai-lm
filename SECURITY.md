<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# Security Policy

Thanks for helping keep AI_LM — and the dealers who run it — safe. This
document explains how to report a vulnerability privately, what is in scope,
how quickly you can expect a response, and the two deployment settings that
most commonly turn a demo into an incident.

## Reporting a vulnerability

**Do not open a public issue, pull request, or discussion for a security
problem.** A public report exposes every operator running the code before a fix
exists. There is no exception to this, not even for "it's only the demo".

Use one of these private channels:

1. **GitHub Security Advisories (preferred).** From this repository, open the
   **Security** tab → **Report a vulnerability**. That starts a private
   advisory visible only to you and the maintainers:
   <https://github.com/FutureBuildAIinc/gable-ai-lm/security/advisories/new>
2. **Email — <colton@futurebuild.ai>.** Send a first contact if you would
   rather arrange an encrypted channel before sharing details. Anonymous
   reports are accepted; if you want credit in the advisory, say how you would
   like to be named.

To help us triage quickly, please include:

- The affected component and path (`backend/internal/...`, `backend/pkg/...`,
  `app/src/...`, `.do/...`).
- The commit SHA or branch you tested against.
- A minimal reproduction, proof of concept, or the vulnerable code path.
- Impact: what an attacker can read, write, or bypass. For AI_LM specifically,
  say whether the finding reaches the **GableLBM write-back path** — that is
  the difference between a bug and an incident.
- Any suggested remediation, if you have one.

## Supported branches

| Branch | Supported | Notes |
|---|---|---|
| `main` | Yes | The only branch today. Security fixes land here first. |
| Forks / vendored copies | No | Rebase onto a patched `main` and re-apply local changes. |

There is no long-term-support line. Run a recent `main` to stay patched.

## Response window

Targets, not contractual guarantees, for a small maintainer team:

| Stage | Target |
|---|---|
| Acknowledge your report | within **3 business days** |
| Initial assessment (severity, affected versions) | within **7 days** |
| Fix or documented mitigation for confirmed high/critical issues | within **90 days**, coordinated with you |

We follow **coordinated disclosure**: we agree a date with you, credit you in
the advisory unless you prefer otherwise, and publish the fix and the advisory
together.

## Scope

**In scope:** everything in this repository — the Go backend under `backend/`,
the Lit frontend under `app/`, the SQL migrations, and the deployment examples
under [`.do/`](.do/).

**Out of scope:** vulnerabilities in third-party dependencies (report those
upstream; the nightly `govulncheck` job in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) tracks them for us), the
non-confidential demo seed data itself, and anything in the upstream GableLBM
ERP — report those to that project.

---

## The two settings that matter

### 1. `AUTH_MODE=dev` must never reach a reachable deployment

AI_LM ships an authentication bypass for local development. When
`AUTH_MODE=dev` is set:

- `backend/cmd/server/main.go` logs `AUTH_MODE=dev: authentication disabled
  (development only)` and **never constructs the auth middleware**. Requests
  therefore carry no claims at all.
- `middleware.RequireRole` in `backend/pkg/middleware/auth.go` treats *nil
  claims* as a pass-through — the comment in that branch says "Dev mode: no
  auth configured, pass through". No user is impersonated; the request is
  simply unauthenticated, and **every role gate opens for it**.

The effect is full dispatcher/admin reach for any anonymous caller. That is
worse for AI_LM than for a read-mostly service, because AI_LM's API is not
read-only: `POST /api/v1/workflow/plans/{id}/push` takes a plan and writes it
into the ERP over `POST /api/integration/delivery-routes` using the deployment's
`GABLE_INTEGRATION_KEY`. An unauthenticated caller who can reach that endpoint
can push arbitrary delivery routes into a live dealer's dispatch board.

**Rules:**

1. **Never set `AUTH_MODE=dev` on a public hostname or a production deploy.**
2. Production must run with `AUTH_MODE` unset (or any value other than `dev`).
   The backend is **fail-closed** in that mode and refuses to start unless:
   - `SESSION_SECRET` is set (it signs and verifies AI_LM staff session JWTs),
   - `DATABASE_URL` is set explicitly — it will not fall back to localhost —
     and uses `sslmode=require`, `verify-ca`, or `verify-full`,
   - `CORS_ORIGINS` is set.
3. `JWKS_URL` is **optional**, and only needed to additionally accept
   externally-issued (shared GableLBM JWKS) tokens. It is not the primary
   verifier — `SESSION_SECRET` is. Older notes in this repo that describe
   `JWKS_URL` as required are out of date.

If you find any reachable environment running `AUTH_MODE=dev`, treat it as a
disclosable vulnerability and report it through the channels above.

> **Known state, disclosed deliberately:** both manifests in [`.do/`](.do/)
> currently set `AUTH_MODE=dev`, and `app-staging.yaml` maps a **public**
> hostname. This is a known, tracked gap, called out at the top of each
> manifest. Those environments carry demo data only. Nothing that requires a
> real customer's data may be deployed from those specs until the checklist in
> [`.do/README.md`](.do/README.md) is completed.

### 2. `GABLE_INTEGRATION_KEY` is a write credential

`GABLE_INTEGRATION_KEY` is sent as `X-Integration-Key` on every call to the
GableLBM integration API — including the route write-back. It is not a
read-only API key. Treat leaking it as equivalent to leaking write access to
the ERP's dispatch board:

- Never inline it in a manifest, a Dockerfile, or a test fixture. It is an
  encrypted platform secret in `.do/` and a `.env` value locally.
- `backend/.env.example` ships it **empty** on purpose. Keep it that way.
- Rotating it requires rotating GableLBM's `INTEGRATION_API_KEY` in lockstep,
  and coordinating with every other consumer of that key.

## Hardening notes

- **`INSTANCE_COUNT` must stay 1.** Parts of the middleware hold in-process
  state; horizontal scaling has not been validated and is not a supported
  configuration today.
- **AI_LM is single-tenant.** There is no tenant column and no tenant scoping
  anywhere in the schema or the query layer. One deployment serves one dealer.
  Do not put two dealers' data in one instance and do not treat the absence of
  a tenant check as a bug to file — it is a documented limitation with a
  roadmap entry.
- **The axle and GVW calculations are engineering heuristics, not a certified
  compliance product.** A wrong answer here is a safety and legal exposure for
  the operator. If you find a case where the solver reports a compliant load
  that is not compliant, that is a security-class report and we want it through
  the private channel, not a public issue.
