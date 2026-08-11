---
# SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
description: Answer which OpenLBM licence governs a file, and what the model and eligibility gates mean.
argument-hint: [a path, or a licensing question]
---

Use the **licensing-check** skill.

Question: $ARGUMENTS

Check this repo's own LICENSE-MAP.md and REUSE.toml first — they are authoritative. Most specific path wins; the connector seam is permissive, `backend/internal/` is not. Never call OpenLBM "open source" without the fair-source qualifier, and always state the draft status when quoting a licence text.
