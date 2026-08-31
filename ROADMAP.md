<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# Roadmap — what AI_LM does not do yet

This document exists to be believed, so it is written to be uncomfortable.

Everything else in this repository describes what AI_LM does. This file lists the
things it **deliberately does not do**, each one a known, located, un-fixed gap
rather than a feature we have not thought of. If you are evaluating AI_LM for a
dealer, a fork, or a licensing conversation, read this first: the rest of the
docs are accurate about the happy path, and this is the rest of the picture.

Nothing here is scheduled. There are no dates, because a date on an unstaffed
item is a lie with a calendar attached. Items are ordered by how likely they are
to matter to you, not by how hard they are.

## The supported model, stated plainly

**AI_LM today is a single-tenant appliance: one deployment, one database, one
dealer.** That is not a limitation we are working around — it is the shape of the
system, top to bottom, and for a self-hosting dealer it is the *right* shape.

**Managed multi-dealer SaaS on one deployment is not supported and cannot be
made to work by configuration.** A managed offering today means one full stack
(app + Postgres + integration key + secrets) per dealer. That is a real,
shippable model; it is just an expensive one, and it is not what most people
mean when they say SaaS.

Every section below is tagged with who it affects:

- **Self-host** — a dealer or integrator running their own AI_LM against their
  own GableLBM.
- **Managed** — FutureBuild (or a licensee) running AI_LM on a customer's
  behalf.

## What this document is *not* about

Several serious defects found in the same review **are** being fixed and are
therefore absent from this list: the ORS null-distance corruption, the hardcoded
Kelowna depot, the plan read-modify-write race, the incomplete push safety gate,
the ignored `stackable` flag, and the zero-rating false PASS. If you are reading
a build where those are still present, this roadmap is older than your checkout.

Two safety statements that are **not** on the roadmap because they are permanent:
the axle and GVW verdicts are engineering heuristics, not certified compliance
output, and no software in this repository relieves anyone of the obligation to
weigh a truck. See `SECURITY.md`.

---

## 1. No multi-tenancy — single-tenant by construction

**Affects: Managed (blocking). Self-host (not applicable, and correct).**

### What it is

There is no tenant, org, dealer or customer dimension anywhere in AI_LM. Not a
column, not a claim, not a context value.

- No migration in `backend/migrations/` defines a tenant column. `vehicle_profiles`,
  `product_dimensions`, `restricted_points`, `load_plans`, `route_plans` and
  `workflow_plans` are keyed by their own id and nothing else.
- The two links back to the ERP are globally unique, not tenant-scoped:
  `gable_vehicle_id UUID NOT NULL UNIQUE` and `gable_product_id UUID NOT NULL UNIQUE`
  (`backend/migrations/001_ai_lm_core.sql`). Two dealers in one database could
  not both register vehicle `abc` even if everything else were scoped.
- One `GABLE_API_URL` / `GABLE_INTEGRATION_KEY` / `SESSION_SECRET` / `DATABASE_URL`
  come from process environment (`backend/internal/config/config.go`) and build
  exactly one `gable.Client` at boot (`backend/cmd/server/main.go`).
- The routing optimizer is a package-level variable installed once at startup:
  `var active SequenceOptimizer` in `backend/internal/routing/sequencer.go`. The
  ORS provider and the `ai.Client` are likewise single process-wide instances.
- `pkg/middleware/auth.go` declares an `OrgID` claim. Nothing in the backend
  reads it.
- Every `.do/*.yaml` component pins `instance_count: 1`.

### Why it matters

The honest version of the sentence "AI_LM is deployable per customer" is: *AI_LM
must be deployed per customer.* A managed offering serving twelve dealers is
twelve apps, twelve databases, twelve secret sets and twelve deploy pipelines,
with no shared control plane and no per-tenant usage data. There is no
configuration, feature flag or middleware that changes this, and adding one
without the schema work underneath would produce cross-dealer data exposure
rather than multi-tenancy.

For a self-hosting dealer this section is a non-issue and arguably a feature:
your data is in your database, your integration key is yours, and there is no
shared plane to be a noisy neighbour on.

### What the fix involves

Roughly two paths, and they are genuinely different products:

- **Formalise silo-per-dealer** (smaller): templated app specs, automated
  provisioning, per-silo secret management and a fleet-of-silos runbook. Changes
  no application code. This is the realistic near-term managed shape.
- **Real multi-tenancy** (large): `tenant_id` on every table with composite
  uniqueness `(tenant_id, gable_vehicle_id)`; tenant-resolution middleware that
  stamps the verified token's org onto the request context; a `tenants` table
  holding per-tenant integration URL, key and depot; tenant-scoped rather than
  process-global optimizer, ORS and AI clients; tenant-scoped caches and rate
  limits; and Postgres RLS or an enforced repository-level scope so a missing
  `WHERE tenant_id` is a compile-or-deny error rather than a data leak. It also
  requires the licensing/metering seam in §4, because a multi-tenant plane you
  cannot meter is a multi-tenant plane you cannot bill.

---

## 2. GableLBM wire types leak past the adapter — "portable to any ERP" is not true in code

**Affects: Self-host (blocking for a non-GableLBM dealer). Managed (blocking for multi-ERP licensing).**

### What it is

The integration adapter is genuinely well built: `backend/internal/gable/client.go`
holds all seven routes, the `X-Integration-Key` header, the bare-JSON-array
decoding and one `do()` helper. The problem is on the other side of it. The
structs that adapter decodes into are the same structs the rest of the system is
written against.

Measured on the current tree: **60 `gable.*` references across 9 non-test files**
outside `internal/gable` (89 across 15 files including tests).

- `backend/internal/routing/service.go` declares its consumer interfaces in terms
  of `gable.Order`, `gable.Vehicle`, `gable.Driver` and `gable.DeliveryRoute`.
- `backend/internal/workflow/service.go` does the same and operates on the wire
  types directly throughout — `analyzeOrder(o gable.Order)`,
  `defaultProfileInput(v gable.Vehicle)`, `usableBedVolume(... gable.Vehicle)`,
  `ensureProfile(... map[string]gable.Vehicle)` — and constructs a
  `gable.DeliveryRoute` for write-back.
- `backend/internal/catalog/service.go` constructs `gable.Product`.
- `backend/internal/auth/service.go` returns `*gable.StaffValidation`.

Four packages — `fleet`, `compliance`, `load` and `ai` — import nothing from
`internal/gable` and are genuinely ERP-agnostic. The pattern works. It was not
applied to the four packages that matter most for portability.

### Why it matters

`ARCHITECTURE.md`, `INTEGRATIONS.md` and `README.md` now all say portability is a
design goal rather than a delivered fact, and point here for the detail. This is
the detail: AI_LM's domain model *is* GableLBM's wire format. Consequences:

- A second ERP adapter cannot be a peer package. It must either emit values of
  structs named after another vendor, or every downstream signature changes.
- GableLBM's wire format and AI_LM's internal model cannot evolve independently:
  a field rename upstream is a refactor across nine files here.
- An ERP whose product/vehicle/order shape differs — different units, no
  per-product geometry, orders without a delivery address — cannot be onboarded
  without editing `routing`, `catalog`, `workflow` and `auth`.

This is the specific reason the portability claim that underpins commercial
licensing should not be made to a third party today. The design is portable; the
code is not yet.

### What the fix involves

A type-substitution and translation boundary, not a redesign — the
consumer-defined interfaces and dependency injection are already correct.

1. Add `backend/internal/erp` with neutral domain types (`Product`, `Vehicle`,
   `Driver`, `Order`, `Route`, `StaffIdentity`) and a `Provider` interface.
2. Demote `internal/gable` to one adapter that translates GableLBM wire shapes
   into port types, and keep every GableLBM-specific quirk (bare JSON arrays,
   nullable geometry pointers, the `X-Integration-Key` header) behind it.
3. Change `routing`, `workflow`, `catalog` and `auth` to depend only on `erp.*`,
   and rename `gable`-prefixed parameters and fields.
4. Keep the `gable_*` database column names or rename them to `external_*` in a
   separate expand/contract migration — that is cosmetic next to the type work
   and should not be bundled with it.

Until this lands, describe AI_LM as *GableLBM-native with a clean adapter seam*,
not as ERP-neutral.

---

## 3. Per-axle numbers are advisory — the datum and overhang model are unvalidated

**Affects: Self-host and Managed equally.**

### What it is

Total gross vehicle weight is sound. `computeAxleLoads` in
`backend/internal/load/solver.go` sums cargo and adds tare
(`plan.TotalWeightLbs = round(cargo) + v.TareWeightLbs`), and the between-axles
split uses the correct static-moment/lever-arm ratio rather than an average. The
unit handling throughout the package is disciplined.

The **per-axle split** is the weak link, for three specific reasons:

- **Datum conflation.** `defaultProfileInput` places the STEER axle at
  `PositionFromFrontIn: 0` (`backend/internal/workflow/service.go`), which is the
  same origin as bed X=0 (`backend/internal/load/model.go`). A real steer axle
  sits under the cab, well ahead of the bed's front edge. Placement X is compared
  directly against axle positions in `distributeToAxles` and `nearestAxle`, so a
  profile built from the default compresses the wheelbase and over-attributes
  cargo to the steer.
- **No overhang model.** Weight behind the last axle is assigned 100% to that
  axle (`distributeToAxles`). In reality a rear overhang acts as a lever that
  *unloads* the steer, so the true rear reaction exceeds 100% of the overhung
  weight and the steer reaction can go negative. Neither is modelled.
- **Tare distribution.** Tare is split proportional to each axle's rating, not by
  the chassis centre of gravity. Convenient, and not the same thing.

There is also an ordering assumption: `distributeToAxles` treats `axles[0]` and
`axles[n-1]` as the span ends and brackets consecutive pairs, but axles are read
back ordered by `axle_number` (`backend/internal/fleet/repository.go`) and are
never re-sorted by position. A tag axle or a mis-numbered profile can bracket the
wrong pair. The default 2-axle flatbed is monotonic, which is exactly why this
never shows up in a demo.

### Why it matters

The headline value proposition is avoiding axle and GVW fines. A per-axle
PASS/WARN/FAIL — the steer axle especially — can be materially wrong in either
direction: a dispatcher may trust a green steer axle and still scale over, or
reject a load that is legally fine. Getting a *confidently wrong* answer is worse
than getting no answer, because the operator stops checking.

### What the fix involves

- Define and document one datum. Capture true wheelbase and steer setback (or a
  per-axle offset from a fixed reference) as first-class fields on the fleet
  profile, and stop deriving axle positions from bed geometry.
- Compute signed axle reactions including front and rear overhang, and assert
  that per-axle cargo sums to total cargo — failing loudly rather than silently
  dropping weight.
- Sort axles by `PositionFromFrontIn` before solving.
- Add table-driven tests for front overhang, rear overhang, 3+ axles and
  weight conservation.
- **Until validated against certified scale tickets, present per-axle numbers as
  advisory with a visible disclaimer.** A per-tenant calibration workflow that
  reconciles computed splits against real weigh-scale tickets is the only thing
  that turns this from a model into a measurement.

### What the dispatch gate does with them today

The push gate matches the confidence stated above, because for a while it did
not: it hard-refused on any per-axle FAIL, which turned this unvalidated estimate
into a hard cap at roughly half a flatbed's rated payload (with the steer axle at
the bed origin, 48-56% of cargo weight lands on it, so a 24 000 lb flatbed failed
its 12 000 lb steer at about 12 500 lb of cargo).

- **Gross weight is exact and still blocks.** Over GVWR, an unrated axle, a
  profile with no GVWR — all refuse a push. That guarantee is untouched.
- **An advisory per-axle over-rating warns and does not block.** It is raised on
  the review step (status WARN, never green), written onto the yard manifest, and
  logged at push. The dispatcher decides — and the thing they should decide to do
  is drive over a scale.
- `load.AxleLoad.Advisory` is the seam. A per-axle verdict from a calibrated or
  scale-ticket-backed source sets it false, and the gate blocks on that again
  with no further change. The zero value is the blocking one.

---

## 4. No licensing, metering or entitlement seam

**Affects: Managed (no billing). Self-host (no enforceable license).**

### What it is

Searching the backend for `licen|entitle|meter|quota|billing|edition` returns
exactly two things: per-staff entitlement delegated to GableLBM's
`validate-staff` (`backend/internal/auth/service.go`,
`backend/internal/gable/client.go`), and a comment in
`backend/migrations/001_ai_lm_core.sql` saying the module is kept portable "for
commercial licensing."

There is no license validation, no offline license key, no usage metering, no
per-tenant counters, no community-vs-commercial feature gate, and no place to put
one. `pkg/metrics` instruments HTTP and the database pool; it counts no business
events at all.

### Why it matters

The commercial thesis — AI_LM licensed to third-party ERP vendors, self-hostable
under OpenLBM Community Source, managed for dealers who do not want to run it —
has no mechanism behind it. You cannot verify that a self-hosted instance is
licensed, meter a managed deployment for billing, or gate a commercial-only
feature. Compliance is on the honour system, which is a fine thing to choose
deliberately and a bad thing to discover.

This is not urgent for the community edition, which is unrestricted by design.
It is a hard prerequisite for either revenue model.

### What the fix involves

Add `backend/internal/license` with three separable pieces:

1. **Entitlement** — a signed license token (issuer, tenant, edition, expiry,
   feature flags) verified at boot, offline, with no phone-home. Self-host needs
   this to work on an air-gapped network.
2. **Metering** — counter events on the events that correlate with value (plan
   created, truck packed, route pushed, catalog pull), exported via `/metrics`
   and/or an outbound usage feed. Needs §1's tenant dimension to be billable.
3. **Feature gating** — a helper handlers consult, so a commercial-only
   capability is one call rather than a fork.

Reserve the seam now even if the community edition gates nothing. Retrofitting
entitlement into a system that has shipped without it is materially harder than
adding an always-true check today.

---

## 5. The integration contract is unversioned prose

**Affects: Self-host and Managed. Blocking for any third-party ERP integration.**

### What it is

The contract a third-party ERP would have to satisfy is `INTEGRATIONS.md` plus
the Go struct tags in `backend/internal/gable/client.go`. That is all of it.

- No OpenAPI document, no JSON Schema, no shared versioned schema package. A
  repo-wide search for `openapi|json-schema|conformance|contract-version` returns
  nothing.
- No contract-version header and no capability probe. `do()` sends
  `X-Integration-Key` and `Accept` and nothing else.
- **A hidden dependency on a specific upstream commit.** The bulk catalog pull
  works because GableLBM commit `b5170de` removed a filter guard that previously
  returned `400` on an unfiltered request. This is documented in prose
  (`INTEGRATIONS.md`, `CLAUDE.md`) and is invisible to code. Point AI_LM at an
  older GableLBM and the catalog silently fails to hydrate.
- No conformance suite. There is nothing an ERP vendor can run to find out
  whether their implementation is correct.

### Why it matters

"Any ERP satisfying the contract can host AI_LM" is currently unverifiable and
unenforceable — and it is the exact seam being sold. An integrator has nothing to
certify against, AI_LM cannot detect an incompatible or older upstream except as
a runtime 4xx/5xx, and the `b5170de` dependency will break a fresh integration in
a way that looks like an empty catalog rather than a version mismatch.

### What the fix involves

- Publish an OpenAPI 3 document (or JSON Schema set) for the six routes, and
  generate the wire structs from it rather than hand-maintaining both.
- Extract the wire schema into a shared, semver-versioned package that both sides
  depend on, so the commit dependency becomes a version constraint.
- Add a contract-version header plus a `GET /api/integration/capabilities` probe,
  checked at boot, with a clear degraded state and a startup log rather than a
  mystery 400 at 6am.
- Ship a black-box conformance harness: point it at an ERP base URL and an
  integration key, and it reports pass/fail per route with the exact payload that
  failed. This is what makes the licensing pitch credible.

---

## 6. No resilience policy at the ERP seam

**Affects: Self-host and Managed equally.**

### What it is

Every AI_LM operation reaches GableLBM live. There is no cache, no retry, no
circuit breaker and no request-scoped memoization.

- `backend/internal/gable/client.go` builds one `http.Client{Timeout: 15s}` and
  `do()` performs a single `c.http.Do`. No retry, no backoff, no breaker.
- The catalog is pulled live on every call. `catalog.ListEffectiveProducts` hits
  `GetProductsWithWeight` each time, and `catalog/handler.go` maps any failure to
  `502 Bad Gateway` — which is also what a local database failure returns, so the
  status cannot distinguish "the ERP is down" from "our database is down".
- Within a single workflow, `ListVehicles` is re-fetched at six call sites
  (`workflow/service.go` in Assign, Pack, Push, repack and profile-ensure paths,
  plus `workflow/review.go`), with `ListDrivers` alongside it. A single plan
  therefore makes four to six identical upstream fleet pulls.

The optional dependencies are handled *well* by comparison: ORS falls back to the
haversine optimizer on any failure, and the OpenRouter client degrades to
"unavailable" rather than failing dispatch. GableLBM has no such fallback,
because it cannot have one — it is the source of truth.

### Why it matters

AI_LM's availability and latency are exactly GableLBM's, with a multiplier and no
buffer. GableLBM deploys on push, so upstream restarts are routine, and during
each one the Load Builder returns 502 and the guided workflow cannot advance. A
single transient blip fails a dispatcher's step outright, and with roughly eight
to twelve sequential upstream calls per workflow run, the probability that at
least one fails is not small on a real network.

Self-hosters feel this most: an appliance that stops working every time the ERP
next to it restarts does not feel like an appliance.

### What the fix involves

Define resilience at the port (which is another reason to do §2 first):

- A short-TTL read-through cache for reference data (products, vehicles, drivers)
  with stale-on-error, surfacing a "catalog may be stale" flag instead of a 502.
- Request-scoped memoization so one workflow operation pulls the fleet once.
- Bounded retries with jittered backoff on idempotent GETs; keep the write-back
  path as-is, since `PushDeliveryRoute` is already idempotent upstream on
  `(vehicle_id, scheduled_date)`.
- A circuit breaker with a documented degraded mode, so a sustained outage fails
  fast with "ERP unavailable" rather than a 15-second hang per call.
- Distinguish upstream from local failures in `catalog/handler.go`: 502 for the
  ERP, 500 for us.

---

## 7. The plan JSONB has no schema version

**Affects: Self-host (sharpest upgrade edge). Managed.**

### What it is

A workflow plan is one JSONB document in `workflow_plans.payload`. The struct it
marshals — `payload` in `backend/internal/workflow/repository.go` — carries no
version field, and `migration 003_workflow_plans.sql` defines no schema-version
column.

Migration `004_workflow_plans_version.sql` adds a `version INTEGER` column, but
that is the **optimistic-concurrency token** for the lost-update fix. It records
that a row changed, not what shape the document inside it is. The two are
unrelated problems.

`unmarshalPayload` uses plain `json.Unmarshal`, so unknown fields are discarded
and missing fields are zero-filled, silently. Any rename, removal or semantic
change to `Plan`, `TruckLoad`, `OrderAnalysis` or `LoadPlan` is applied to
in-flight documents on the next read-modify-write, with no error and no signal.

### Why it matters

This is the failure mode of an ordinary Tuesday. A dealer running
`git pull` + redeploy at 11am — which is exactly what a self-hoster does, and
what DigitalOcean's `deploy_on_push` does automatically — silently degrades every
plan that is currently open. A field that moved becomes a zero. A truck's
compliance review or proof-of-load quietly empties. Nothing errors; the next save
just writes the lossy version back.

It is worse for self-hosters because there is no vendor operations team watching
for it, and worse still because the symptom (a plan that looks subtly wrong)
arrives hours after the cause.

### What the fix involves

- Add `payload_schema_version` both as a column and inside the document, and
  stamp it on write.
- Gate `unmarshalPayload` on it: known version → read; older version → run a
  forward migration function; newer version → refuse and say so, rather than
  silently truncating a document written by a newer build.
- Write forward-migration functions for the JSONB alongside any struct change,
  and treat "did you bump the payload version?" as a review question.
- Document a drain runbook: complete or close open plans before upgrading. Cheap,
  and it covers the gap until the versioning lands.

---

## Smaller known gaps, listed so they are not surprises

These are real and located, but each is a contained fix rather than a structural
one:

- **Securement rulesets are compiled in.** `backend/internal/load/securement_rules.go`
  registers `US_FMCSA` and `CA_NSC` as Go values. Another jurisdiction is a code
  change and a redeploy, which is a poor fit for self-hosting.
- **Tie-down count is derived from the whole-load span**, not per article length,
  while citing FMCSA §393.110 and NSC Standard 10 — which count per article. For
  a load of many short heavy pieces the recommendation can be too low.
- **No per-call observability on the paid ORS dependency.** A silent downgrade
  from road distances to straight-line estimates is one `slog.Warn`; the
  over-ceiling fallback logs nothing at all.
- **Scheduled auto-lock uses server-local time.** `parseLockTime` resolves lock
  windows with `time.Local`, containers run UTC, and there is no timezone in the
  workflow config — so a "06:00" lock fires at 22:00 or 23:00 local for a
  Pacific-timezone yard.
- **The AI briefing sends customer and driver names to OpenRouter** with no
  zero-data-retention routing, no provider allowlist and no redaction. It is off
  unless a key is configured, and it should stay off in any deployment that has
  not had that conversation. Self-hosters can point `OPENROUTER_BASE_URL` at a
  local OpenAI-compatible server and keep the data on-premises — a genuine
  strength, currently undocumented and untested.

---

## How to use this document

If you are **self-hosting**: §3 (advisory axle numbers), §6 (your Load Builder
502s when your ERP restarts) and §7 (upgrading mid-day) are the three that will
reach you first. §1 does not apply to you. §2 applies only if your ERP is not
GableLBM, in which case it applies completely.

If you are **evaluating a managed offering**: §1 is the answer to "can you host
us on shared infrastructure" and the answer is no, one stack per dealer. §4 is
the answer to "how do you bill for this" and the answer is not yet.

If you are an **ERP vendor considering an integration**: §2 and §5 are the two
that decide whether this is a weekend or a quarter. Ask for the conformance
harness; if it does not exist yet, that is the state of the world and this
document is where we said so.
