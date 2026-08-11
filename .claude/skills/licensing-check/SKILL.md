---
# SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
name: licensing-check
description: Answer which OpenLBM licence governs a file in Gable AI_LM and explain the per-component model and the three eligibility gates in plain language. Use when the user says "what licence is this", "which SPDX header do I need", "can my company use this", "do I have to open-source my changes", "is this open source", "what's the difference between Commons and Connector", "can I use the Gable name", "/license-of".
---

# licensing-check — which licence governs this, and what it means

Two different questions. Work out which one before answering.

1. **Mechanical:** "what SPDX header does this file need?" → §1, answerable exactly.
2. **Substantive:** "may my company use this / must I publish my changes?" → §3, where you
   explain the model and then decline to give legal advice.

---

## 1 · Files in this repository

**Gable AI_LM** is governed by **Community-Source (free for community members, fee for others; applied per work, by version notice — not per directory)**; documentation and anything under `.claude/`
is `LicenseRef-OpenLBM-Docs-1.0`.

**Check the repo's own metadata first — it is authoritative over this skill**, especially while
the repo is being built out:

```bash
ls LICENSE LICENSE-MAP.md REUSE.toml LICENSES/ 2>/dev/null
cat LICENSE-MAP.md 2>/dev/null
head -3 <path/to/file>          # the file's own header
```

Order of authority: the file's own `SPDX-License-Identifier` → `REUSE.toml` → `LICENSE-MAP.md`.

Header formats:

<!-- REUSE-IgnoreStart -->

```go
// SPDX-License-Identifier: <identifier>
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
```

<!-- REUSE-IgnoreEnd -->

`//` for Go and TypeScript, `--` for SQL, an HTML comment for Markdown, `#` comments **inside**
YAML frontmatter for files that have any, and a `<filename>.license` sidecar for anything that
can't hold a comment. A `reuse lint` gate is part of this ecosystem's CI — a file with no
licensing information fails the build.

## 2 · The Standard

OpenLBM is a **fair-source** framework written for this project — **not** an OSI-approved
open-source licence. Say that plainly whenever "open source" comes up; people make real
decisions on it.

| Profile | SPDX identifier | Covers | Roughly |
|---|---|---|---|
| **Commons 1.0** | `LicenseRef-OpenLBM-Commons-1.0` | Core ERP / the commons | Reciprocity conditioned on size |
| **Surface 1.0** | `LicenseRef-OpenLBM-Surface-1.0` | Client surfaces, native apps | Reciprocity on distribution |
| **Connector 1.0** | `LicenseRef-OpenLBM-Connector-1.0` | The plug-in seam / Module SDK | Permissive, **no copyleft** |
| **Community-Source 1.0** | `LicenseRef-OpenLBM-Community-Source-1.0` | Community satellites | Free for members, fee for others; applied **per work**, not per directory |
| **Docs 1.0** | `LicenseRef-OpenLBM-Docs-1.0` | Documentation and specs | Docs terms |
| **Trademark** | `LicenseRef-OpenLBM-Trademark` | The Gable marks | A brand-use **policy** — grants no code rights |

In the reference implementation (`FutureBuildAIinc/gable`), the mapping is by directory, and
**the most specific path wins**: `backend/pkg/apps/` is carved out of the `backend/pkg/`
Commons default and is **Connector**, so third parties can plug in without copyleft crossing
the boundary. Getting that backwards is the most common licensing mistake in the ecosystem.

## 3 · The three eligibility gates

Separate tests; failing any one changes the answer.

1. **Size** — Independent Operator versus Large Operator. The commons is built for independent
   dealers; consolidators are gated (contribute back or pay).
2. **Field-of-use** — a Competing Vendor / Competing Use exclusion. A legacy LBM software vendor
   is gated **regardless of size**. Being small doesn't help if you're building a competing
   product.
3. **Participation** — Community Member status governs **governance and joint-venture rights**,
   **not the grant**. Never tell anyone they must join something to use the software.

Canonical definitions live in `FutureBuildAIinc/openlbm` and **supersede any summary, including
this one**. Quote from there, not from here, when it matters.

## 4 · Common questions

**"Is this open source?"** No — fair source. Public, forkable, self-hostable, but the grants are
conditional.

**"Can my company use it?"** Depends on all three gates and on which components. Walk them
through §3, then: *this is not legal advice — have counsel read the Standard.*

**"If I write a plug-in, must I publish it?"** The Connector seam (`FutureBuildAIinc/gable-sdk`,
and `backend/pkg/apps/` in the host) exists so the answer can be no. But a plug-in that imports
`backend/internal/...` has reached into Commons. Point at the boundary, then at counsel.

**"What licence is my contribution under?"** The same one governing the files you changed, via a
Contributor License Agreement you'll be asked to sign before your first merge.

**"Can I use the Gable name or logo?"** Separate instrument — `LicenseRef-OpenLBM-Trademark`,
with assets and the policy in `FutureBuildAIinc/brand`. **No code licence grants trademark
rights.**

**"Can I copy in code from elsewhere?"** Only if you have the right to license it under the
profile it would land in, and you say where it came from in the PR. Never paste in competitor
documentation, schemas, or API specifications.

## 5 · Draft status — state it every time

Licence texts under `LICENSES/` in these repos are **drafts pending counsel review** and are not
effective legal instruments. The canonical Standard is at
<https://github.com/FutureBuildAIinc/openlbm>; where they disagree, the published Standard
governs.

## 6 · If the mapping looks wrong

A file whose header disagrees with `LICENSE-MAP.md`/`REUSE.toml`, or a directory not covered at
all, is a real finding. **Don't edit `REUSE.toml` or `LICENSE-MAP.md` to fix it yourself** —
those are maintainer-owned. Use **`report-an-issue`** with the exact file, the header it has,
and the header the map implies.

---

## Ground rules

- **You are not counsel.** Explain the model; for anything with money attached, point at the
  canonical Standard and a lawyer.
- **Never call OpenLBM "open source"** without the fair-source qualifier.
- **Check the repo's own metadata first** — it is authoritative over this skill.
- **Most specific path wins.**
- **Participation is not the grant.**
- **Trademark is separate.**
- **Always state the draft status** when quoting a licence text.
