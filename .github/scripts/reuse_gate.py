#!/usr/bin/env python3
# SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
"""REUSE compliance gate for the gable-ai-lm repository.

Runs `reuse lint --json` and fails on every REUSE non-compliance category
*except* `unused_licenses`, which is reported as a note instead.

What this actually enforces
---------------------------
Every file in the tree must carry copyright and licensing information, every
license it names must have a text in `LICENSES/`, no license may be deprecated
or unrecognised, and every SPDX expression must parse. Those are hard
failures. LICENSE-MAP.md and REUSE.toml describe *what* the licensing is; this
script is the thing that stops the description from drifting away from the
tree.

Why the one carve-out
---------------------
`LICENSES/` is a *license catalog*, not merely the set of licenses that happen
to govern files in this repo. Today the catalog is exactly the two texts in
use — Community Source and Docs — so `unused_licenses` is empty and this
exception is dormant. It is kept anyway, and deliberately: the moment someone
mirrors in a catalogued-but-not-directory-scoped text (the OpenLBM trademark
policy is the obvious candidate, and the host repo already ships several),
plain `reuse lint` would start exiting 1 for a licensing *improvement*. The
REUSE specification has no way to express "this text is catalogued but
unused", so the gate has to make that distinction itself.

If the maintainers ever decide the catalog must contain nothing but licenses
in active use, delete the TOLERATED set below and this script collapses into a
straight `reuse lint`.

Run it locally with `python3 .github/scripts/reuse_gate.py` from the repository
root, after `pipx install reuse==6.2.0`. Set `REUSE_BIN` if the executable is
somewhere unusual.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

# The only REUSE non-compliance category that does not fail this gate.
TOLERATED = {"unused_licenses"}

# Human-readable labels for the categories reuse reports. Any category reuse
# adds in a future version is picked up automatically and treated as a hard
# failure (it just gets a raw key as its label) unless it is added to TOLERATED.
LABELS = {
    "bad_licenses": "Unrecognised / non-SPDX licenses",
    "deprecated_licenses": "Deprecated SPDX licenses",
    "licenses_without_extension": "License files without a file extension",
    "missing_licenses": "Licenses referenced but missing from LICENSES/",
    "unused_licenses": "License texts in LICENSES/ not used by any file",
    "read_errors": "Files that could not be read",
    "missing_copyright_info": "Files with no copyright information",
    "missing_licensing_info": "Files with no licensing information",
    "invalid_spdx_expressions": "Files with an unparseable SPDX expression",
}


def invalid_expressions(report: dict) -> list[str]:
    """Files whose SPDX expression does not parse.

    reuse 6.2 checks this in `reuse lint` but does *not* surface it under
    `non_compliant` in the JSON report -- it only shows up as `is_valid: false`
    on the per-file expressions. Without this, the gate would pass a tree that
    plain `reuse lint` rejects.

    The usual cause is documentation: prose or a shell snippet that quotes an
    SPDX license tag, which reuse then scrapes as if it were the file's real
    tag. The fix is to bracket the offending block with reuse's ignore markers
    (see <https://reuse.software/faq/#exclude-file>), not to reword the docs.
    """
    found = []
    for entry in report.get("files", []):
        for expression in entry.get("spdx_expressions", []):
            if not expression.get("is_valid", True):
                found.append(f"{entry['path']}: {expression.get('value')!r}")
    return found


def emit(lines: list[str]) -> None:
    """Print to stdout and, on GitHub Actions, to the job step summary."""
    text = "\n".join(lines)
    print(text)
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as handle:
            handle.write(text + "\n")


def main() -> int:
    reuse_bin = os.environ.get("REUSE_BIN", "reuse")
    try:
        proc = subprocess.run(
            [reuse_bin, "lint", "--json"],
            capture_output=True,
            text=True,
            check=False,
        )
    except FileNotFoundError:
        print(
            f"error: '{reuse_bin}' not found. Install it with "
            "`pipx install reuse` or `pip install reuse`.",
            file=sys.stderr,
        )
        return 2

    if not proc.stdout.strip():
        sys.stderr.write(proc.stderr)
        print("error: `reuse lint --json` produced no output", file=sys.stderr)
        return 2

    report = json.loads(proc.stdout)
    non_compliant = dict(report.get("non_compliant", {}))
    summary = report.get("summary", {})

    # Not reported under non_compliant by reuse 6.2 -- see invalid_expressions().
    non_compliant["invalid_spdx_expressions"] = invalid_expressions(report)

    failures = {
        key: value
        for key, value in non_compliant.items()
        if value and key not in TOLERATED
    }
    warnings = {
        key: value
        for key, value in non_compliant.items()
        if value and key in TOLERATED
    }

    lines = [
        "## REUSE licensing gate",
        "",
        f"- reuse {report.get('reuse_tool_version', '?')}, "
        f"spec {report.get('reuse_spec_version', '?')}",
        f"- Files scanned: **{summary.get('files_total', '?')}**",
        f"- With copyright info: **{summary.get('files_with_copyright_info', '?')}**",
        f"- With licensing info: **{summary.get('files_with_licensing_info', '?')}**",
        f"- Licenses in use: {', '.join(summary.get('used_licenses', [])) or 'none'}",
        "",
    ]

    for key, value in warnings.items():
        lines.append("> [!NOTE]")
        lines.append(f"> {LABELS.get(key, key)} (tolerated, not a failure):")
        for item in value:
            lines.append(f"> - `{item}`")
        lines.append("")

    if failures:
        lines.append("### Failures")
        lines.append("")
        for key, value in failures.items():
            lines.append(f"**{LABELS.get(key, key)}** ({len(value)}):")
            lines.append("")
            for item in value[:50]:
                lines.append(f"- `{item}`")
            if len(value) > 50:
                lines.append(f"- ... and {len(value) - 50} more")
            lines.append("")
        emit(lines)
        for key in failures:
            print(f"::error::REUSE gate failed: {LABELS.get(key, key)}")
        return 1

    lines.append("REUSE gate **passed**.")
    emit(lines)
    return 0


if __name__ == "__main__":
    sys.exit(main())
