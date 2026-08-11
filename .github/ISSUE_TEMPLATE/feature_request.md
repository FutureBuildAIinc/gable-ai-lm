---
name: Feature request
about: Suggest an idea or improvement for AI_LM
title: "[Feature] "
labels: enhancement
assignees: ''
---

<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

Please check ROADMAP.md first — several of the obvious gaps are already
planned, and a comment on an existing item is more useful than a duplicate.
-->

## Problem / motivation

What problem does this solve? Describe the real dispatch, yard, or compliance
pain point — "our dispatcher spends 40 minutes every morning re-sequencing
stops by hand because..." is far more useful than "add a re-sequence button".

## Proposed solution

A clear and concise description of what you want to happen.

## Affected area

Which component would this touch?

- [ ] Workflow / guided dispatch (`backend/internal/workflow`)
- [ ] Load solving, 3D packing, or securement (`backend/internal/load`)
- [ ] Route optimization (`backend/internal/routing`)
- [ ] Compliance and restricted points (`backend/internal/compliance`)
- [ ] Fleet or catalog data (`backend/internal/fleet`, `internal/catalog`)
- [ ] The GableLBM integration contract (`backend/internal/gable`)
- [ ] Frontend (`app/`)
- [ ] Deployment / operations (`.do/`, CI)

## Alternatives considered

Other approaches you thought about, and why you prefer the proposal.

## Scope & compatibility

- Does this need a database migration?
- Does it change the `/api/v1/*` surface AI_LM serves?
- Does it need anything **new from GableLBM** — a new `/api/integration/*`
  route, or a new field on an existing one? If so this becomes a two-repo
  change and needs a matching issue on the host repo.
- Any backward-compatibility concerns for existing plans or manifests?

## Compliance impact

- [ ] This changes nothing about axle, GVW, volume, or securement calculations.
- [ ] This changes those calculations. (If so: what jurisdiction or standard is
      the authority for the new behaviour, and how should it be tested?)

## Additional context

Mockups, references to how existing dispatch software handles this, photos of
the paper process it would replace, links to related issues — anything that
helps.
