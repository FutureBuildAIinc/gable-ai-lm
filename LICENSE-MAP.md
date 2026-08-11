<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# License Map

AI_LM is licensed **per component**, like every repository in the Gable
ecosystem — but AI_LM's map is deliberately short. The full text of each
license lives in [`LICENSES/`](LICENSES/), every file additionally carries a
matching `SPDX-License-Identifier` header (or a `.license` sidecar, for formats
that cannot hold a comment), and [`REUSE.toml`](REUSE.toml) encodes the same
mapping in machine-readable form. `.github/workflows/ci.yml` enforces it on
every pull request.

| Path prefix | SPDX license identifier | License text |
|---|---|---|
| `backend/` | `LicenseRef-OpenLBM-Community-Source-1.0` | [LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt](LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt) |
| `app/` | `LicenseRef-OpenLBM-Community-Source-1.0` | [LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt](LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt) |
| `.do/`, `docker-compose.yml` | `LicenseRef-OpenLBM-Community-Source-1.0` | [LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt](LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt) |
| Root `*.md` (docs, guides, policies) | `LicenseRef-OpenLBM-Docs-1.0` | [LICENSES/LicenseRef-OpenLBM-Docs-1.0.txt](LICENSES/LicenseRef-OpenLBM-Docs-1.0.txt) |
| `.github/`, `.claude/`, ignore files | `LicenseRef-OpenLBM-Docs-1.0` | [LICENSES/LicenseRef-OpenLBM-Docs-1.0.txt](LICENSES/LicenseRef-OpenLBM-Docs-1.0.txt) |

[`LICENSE`](LICENSE) at the repository root is a copy of the Community Source
text — the license that governs the software itself.

## Why this map is flat, and how it differs from `gable`

The host repository, [`gable`](https://github.com/FutureBuildAIinc/gable),
splits its tree across four licenses: **Commons** for the ERP core, **Surface**
for client apps, **Connector** for the third-party integration seam, and
**Docs** for prose. That split exists because `gable` is a commons that
third parties are expected to build *on top of* and *plug into*, so the licence
has to change at each of those boundaries — most importantly at
`backend/pkg/apps/`, where copyleft must stop so an outside app can attach
without being infected.

AI_LM has none of those boundaries. It is a **satellite**: a single deployable
product that consumes the host's integration API and has no plug-in seam of its
own. There is nothing here for a third party to extend from the inside, so
there is nothing to carve out. One Licensed Work, one license.

## What Community Source actually means here

`LicenseRef-OpenLBM-Community-Source-1.0` is a **source-available** license, not
an OSI open-source license. In plain terms:

- **The source is public.** Anyone may read it, modify it, build it, run it for
  development, testing, and evaluation, and redistribute it with the license
  intact. That is Section 1 and it applies to everyone.
- **Community Members may run it in production, free, immediately.** The
  license defines a Community Member as an organisation operating fewer than
  **50 locations** that is not controlled by (and does not control) an entity
  above **$1B** annual revenue. If that is you — and it is most independent LBM
  dealers — you may use AI_LM to run your business at no charge, today, with no
  Change Date to wait for.
- **Everyone else needs a commercial license to run it in production.** Large
  chains, national buying groups, and anyone offering AI_LM as a hosted or
  managed service to third parties are outside the community carve-out. They
  keep every other right — read, modify, evaluate, redistribute — but
  production use is fee-bearing.
- **It converts to AGPL-3.0-only after five years**, per released version, on
  that version's own Change Date.

This is a different shape from the host repo's Commons license, and the
difference is intentional. `gable` is the commons that the industry co-owns.
AI_LM is the value-added satellite that funds it: **free for the community, fee
for the giants.** Both are source-available; only the production-use condition
differs.

The definitive terms are in the license text, not in this summary. Read
[LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt](LICENSES/LicenseRef-OpenLBM-Community-Source-1.0.txt)
before relying on any of the above.

## Prose

`LicenseRef-OpenLBM-Docs-1.0` covers the documentation and repository
plumbing — README, architecture and deploy notes, contributor and security
policy, CI workflows, issue templates, the Claude contributor kit.

CI config is Docs rather than Community Source on purpose. A GitHub Actions
workflow is not part of the shipped product; nobody "uses AI_LM in production"
by running our test matrix. Applying a license whose central term is a
production-use condition to a file that can never be used in production would
be noise in the map. The deployment manifests under [`.do/`](.do/) go the other
way for the same reason inverted — they *are* how you run the product, they
ship with it, and an operator edits them the way they edit code.

## Trademark

The Community Source license grants **no** rights in the names or logos
"Gable", "GableLBM", "AI_LM", "FutureBuild", or "OpenLBM" (Section 6). Brand
use is governed by the separate trademark policy in the
[OpenLBM Standard](https://github.com/FutureBuildAIinc/openlbm), which is not
mirrored into this repository. A fork must be renamed.

## Contributions

Inbound contributions are licensed under the same license that governs the
file(s) you change, via a CLA. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

The OpenLBM license texts under `LICENSES/` are **drafts pending counsel** and
are not yet effective as final legal instruments — the Community Source text
still carries unresolved `[COUNSEL: ...]` markers on its termination mechanics,
governing law, and the 50-location / $1B thresholds. The canonical Standard is
at <https://github.com/FutureBuildAIinc/openlbm>; where it and these drafts
disagree, the published Standard governs.
