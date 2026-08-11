---
# SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
description: Run the pre-flight before opening a PR — whatever gates the repo defines, plus SPDX, secrets, and PR target.
argument-hint: [optional: a path or area to focus on]
---

Use the **check-my-contribution** skill.

Focus: $ARGUMENTS

Discover the gates first — read the Makefile and the CI workflow and run exactly what CI runs. Never report a gate as passing if you did not execute it.
