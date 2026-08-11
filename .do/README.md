<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# `.do/` — DigitalOcean App Platform manifests

Two specs live here. **Neither is a production spec.** Both set
`AUTH_MODE=dev`, and one of them serves a public hostname.

| Spec | Branch | DO app | State |
|---|---|---|---|
| [`app-staging.yaml`](app-staging.yaml) | `main` | `ai-lm-staging` | **Live** at <https://load.gablelbm.com>. Demo data, wired to GableLBM's demo ERP. |
| [`app-demo.yaml`](app-demo.yaml) | `community` | `ai-lm-demo` | **Not provisioned.** Neither the app nor the branch exists. A reviewed template. |

Operational detail — app IDs, `doctl` runbook, rollback, and the current
integration-key mismatch — is in [`../DEVOPS.md`](../DEVOPS.md). This file
covers only what must be true before either spec is pointed at real data.

---

## Why `AUTH_MODE=dev` is still here

Because removing it from a running demo would break the demo without making
anything safe.

`AUTH_MODE=dev` disables authentication entirely: the middleware is never
constructed, requests carry no claims, and `RequireRole` passes anything
through. Details and the exact code paths are in
[`../SECURITY.md`](../SECURITY.md). AI_LM's API is not read-only — it pushes
delivery routes into the ERP — so on a public hostname this is a genuine
unauthenticated write API.

Flipping the flag in isolation would:

1. **Fail the deploy.** With `AUTH_MODE` unset the server is fail-closed and
   exits unless `SESSION_SECRET`, `CORS_ORIGINS`, and an explicit
   `DATABASE_URL` with a secure `sslmode` are all present.
2. **Lock out the demo** even if it did start, because staff login goes through
   GableLBM's `/api/integration/validate-staff` and there are no entitled demo
   staff accounts on the demo ERP to log in as.

So the flag stays, the exposure is documented at the top of both manifests, and
the environments are kept to disposable data. Changing that is a sequenced
piece of work, not a one-line edit — which is exactly why it is written down
here rather than done quietly.

---

## Production checklist

Every box below must be ticked **before** any deployment holds a real dealer's
orders, customers, or fleet. Copy one of these specs to a new file
(`app-production.yaml`); do not repurpose the demo ones.

### Authentication

- [ ] **Remove the `AUTH_MODE` env var entirely** from the backend service.
      Do not set it to `prod` or `production` — any value other than `dev`
      works, but absence is unambiguous.
- [ ] Set `SESSION_SECRET` as an encrypted secret. It signs and verifies AI_LM
      staff session JWTs. Generate with `openssl rand -hex 32`. The server
      refuses to start without it.
- [ ] Set `CORS_ORIGINS` to the exact frontend origin. Also required; the
      server refuses to start without it.
- [ ] Confirm staff can actually log in: `POST /api/v1/auth/login` calls the
      ERP's `/api/integration/validate-staff`, so the connected GableLBM must
      have entitled staff records with `modules` including AI_LM. Verify this
      **before** cutover, not after.
- [ ] Optionally set `JWKS_URL` and `AUTH_ISSUER` to additionally accept
      externally-issued tokens. This is optional — `SESSION_SECRET` is the
      primary verifier. Any documentation saying `JWKS_URL` is required is out
      of date.

### Data

- [ ] **Do not set `DEMO_SEED`.** Its absence is what keeps the demo dealer's
      two fictional trucks and three fictional restricted points out of the
      database. See [the seeding contract](#the-seeding-contract) below.
- [ ] Point `DATABASE_URL` at a dedicated database. AI_LM is single-tenant —
      one deployment, one dealer. There is no tenant column anywhere.
- [ ] Confirm the `sslmode` on the connection string is `require`,
      `verify-ca`, or `verify-full`. The server appends `sslmode=require` when
      none is present and refuses to start on an insecure explicit value.

### Integration

- [ ] `GABLE_API_URL` points at the customer's own GableLBM instance, not a
      demo.
- [ ] `GABLE_INTEGRATION_KEY` is an encrypted secret matching **that**
      instance's `INTEGRATION_API_KEY`. It is a **write** credential — the
      route write-back uses it. Never share one key across environments.

### Operations

- [ ] `instance_count` stays `1`. Parts of the middleware hold in-process
      state; horizontal scaling is unvalidated.
- [ ] Alerts beyond `DEPLOYMENT_FAILED` / `DOMAIN_FAILED` are configured —
      at minimum a probe on `/healthz/ready` and something watching the
      `/metrics` endpoint.
- [ ] A backup and restore path for the Postgres cluster has been tested, not
      merely enabled.
- [ ] Someone has read [`../SECURITY.md`](../SECURITY.md) and understands that
      the axle and GVW verdicts are engineering heuristics, not certified
      compliance output.

---

## The seeding contract

The `migrate-and-seed` POST_DEPLOY job in both specs runs:

```sh
./migrate && if [ "$DEMO_SEED" = "true" ]; then ./seed; fi
```

**Migrations always run. The demo seed only runs when `DEMO_SEED` is exactly
the string `"true"`.**

This used to be an unconditional `sh -c "./migrate && ./seed"`, which meant
every deploy of every environment re-injected one demo dealer's fleet profiles
and restricted points — fine on a demo, actively wrong anywhere else. A real
dealer would find two fictional trucks in their fleet list after every deploy.

The gate is **opt-in**. A new environment that simply does not declare
`DEMO_SEED` gets migrations and nothing else, which is the safe default and
requires no vigilance from whoever writes the next spec.

Both current specs set `DEMO_SEED: "true"`, because both currently hold
disposable demo data and that is the behaviour they have today. The value must
be removed from `app-staging.yaml` the moment that environment carries anything
other than demo data. It stays in `app-demo.yaml` — a community demo that
cannot reset itself is not a demo.

The seed is idempotent. Vehicle profiles upsert on `gable_vehicle_id`, and
restricted points upsert on a case- and whitespace-normalised name, so a repeat
deploy converges rather than adding a duplicate Bennett Bridge. Re-running it is
safe. It also warns when the registry holds points the seed does not own, which
is the signal that you have pointed `DEMO_SEED` at a real dealer's database.

### Why the job also sets `AUTH_MODE=dev`

The `migrate` and `seed` binaries load the **server's** full config, which is
fail-closed when `AUTH_MODE != "dev"` and demands `SESSION_SECRET`. Without
`AUTH_MODE=dev` on the job, it exits with
`FATAL: SESSION_SECRET must be set` before running a single migration.
(`app-demo.yaml` was missing this line entirely and would have failed on first
provision — that is fixed here.)

The job serves no HTTP, so this is not an exposure. It is still a workaround:
the right fix is for `cmd/migrate` and `cmd/seed` to load only the
configuration they actually use. A production spec that follows the checklist
above will need either that fix, or a `SESSION_SECRET` on the job too.

---

## Left deliberately for a human

These are not oversights. They need a decision or a credential that does not
belong in a repository:

1. **Flipping `AUTH_MODE` on the live demo.** Sequenced work, not an edit —
   see [above](#why-auth_modedev-is-still-here).
2. **Repointing the DO apps at the new repository.** AI_LM moved from
   `futurebuildai/AI_LM` to `FutureBuildAIinc/gable-ai-lm`. Both specs now name
   the new repo, but App Platform cannot follow until the DigitalOcean GitHub
   App is granted access to it, and `app-demo.yaml` additionally needs a
   `community` branch to exist. Push first, grant access second, `doctl apps
   update` third.
3. **The `GABLE_INTEGRATION_KEY` mismatch on `ai-lm-staging`.** Ingest against
   `demo.gablelbm.com` returns `401 invalid integration key`. Both sides are
   encrypted secrets, so the fix needs someone holding the value — see
   [`../DEVOPS.md`](../DEVOPS.md).
4. **Whether `ai-lm-demo` should exist at all**, given `ai-lm-staging` already
   serves a public demo at `load.gablelbm.com`. Two public demos of the same
   pre-GA service on two hostnames may be one more than anyone wants to keep
   patched.
