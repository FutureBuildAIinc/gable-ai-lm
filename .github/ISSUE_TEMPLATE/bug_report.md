---
name: Bug report
about: Report something that isn't working as intended
title: "[Bug] "
labels: bug
assignees: ''
---

<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

STOP if this is a security issue — do not file it here. See SECURITY.md and
report privately. That includes any case where the load solver reports a load
as compliant when it is not.
-->

## Description

A clear and concise description of what the bug is.

## Affected area

Which part of AI_LM? For example:

- a workflow step (Ingest / Assign / Pack / Route Review / Manifest & Push)
- a backend module (`backend/internal/load`, `internal/routing`,
  `internal/compliance`, `internal/workflow`, `internal/catalog`,
  `internal/fleet`, `internal/gable`)
- a frontend page (`app/src/pages/...`) or the 3D visualiser
- the GableLBM integration, or a `.do/` deployment manifest

## Steps to reproduce

1. Go to '...'
2. Click on '...'
3. See error

## Expected behavior

What you expected to happen.

## Actual behavior

What actually happened. Include the exact error message and stack trace if any.

## Environment

- Commit SHA or branch:
- Go version (`go version`):
- Node version (`node -v`):
- PostgreSQL version:
- How you're running it (docker compose / self-hosted / DigitalOcean / other):
- `AUTH_MODE` value (`dev` or unset):
- Is `GABLE_API_URL` pointed at a live GableLBM instance, or is the integration
  unconfigured?

## Load / route context (if the bug involves a plan)

This is usually the fastest path to a reproduction. Please redact customer
names and addresses first.

- Vehicle profile: bed L/W/H, GVWR, axle count and ratings
- What was on the truck (SKUs, quantities, per-unit weights and dimensions)
- What the solver reported (axle weights, GVW, pass/warn/fail)
- What you believe the correct answer is, and how you calculated it

## Logs, screenshots, or reproduction

Paste relevant backend logs, browser console output, or screenshots. Redact
secrets, customer data, and `GABLE_INTEGRATION_KEY` first.

## Additional context

Anything else that might help — recent changes, related issues, workarounds you
tried.
