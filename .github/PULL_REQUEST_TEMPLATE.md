<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

Thanks for contributing to AI_LM!

Target branch: `main`. See CONTRIBUTING.md.
-->

## Summary

What does this PR do, and why? Link any related issue (e.g. `Closes #123`).

## Area(s) touched

- [ ] `backend/internal/` — a domain module (which one?)
- [ ] `backend/internal/gable/` — the GableLBM integration contract
- [ ] `backend/internal/load/` or `internal/compliance/` — **axle / GVW /
      securement math** (see the safety checklist below)
- [ ] `backend/migrations/` — schema
- [ ] `app/` — the Lit frontend
- [ ] `.do/` — deployment manifests
- [ ] `.github/` — CI or repo plumbing
- [ ] Docs only

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor / cleanup
- [ ] Documentation
- [ ] Database migration
- [ ] Other (describe):

## Pre-flight checklist

CI runs exactly these. Failing locally is faster than failing in Actions.

**Backend** (`cd backend`):

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...`
- [ ] `go build ./...`
- [ ] `go test -race ./...`

**Frontend** (`cd app`):

- [ ] `npm ci`
- [ ] `npx tsc --noEmit`
- [ ] `npm run lint --if-present`
- [ ] `npm run test`
- [ ] `npm run build`

**Licensing** (repo root):

- [ ] `python3 .github/scripts/reuse_gate.py` passes (needs
      `pipx install reuse==6.2.0`)
- [ ] Every new file carries an SPDX header matching `LICENSE-MAP.md`

**General:**

- [ ] This PR targets `main`.
- [ ] Commits are focused, with clear messages.
- [ ] New endpoints are wired into the module's `RegisterRoutes` and reachable
      from `backend/cmd/server/main.go`.
- [ ] New migrations are `NNN_name.sql` with a sibling `NNN_name_down.sql`, and
      are additive (no edits to already-applied migrations).
- [ ] I have read `CONTRIBUTING.md` and agree to license my contribution under
      the OpenLBM license governing the file(s) I touched (via the CLA).
- [ ] No secrets, credentials, live hostnames, or customer data are committed.
- [ ] `AUTH_MODE=dev` is not introduced into any reachable config.

## Safety checklist — only if you touched load, axle, GVW, or securement math

These outputs go to a driver who is about to put a real truck on a real road.

- [ ] The change is covered by a unit test with a hand-checked expected value,
      not a value copied from the new implementation's own output.
- [ ] Units are explicit and consistent (pounds, inches, decimal degrees).
- [ ] A load that was previously reported as **non-compliant** cannot become
      compliant as a side effect of this change — or, if it can, the PR
      description explains exactly why the old answer was wrong.
- [ ] Rounding moves in the conservative direction.

## Screenshots / notes for reviewers

(Optional) UI screenshots, migration notes, or anything reviewers should know.
