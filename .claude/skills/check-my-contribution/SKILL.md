---
# SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
name: check-my-contribution
description: Run the pre-flight before opening a pull request against Gable AI_LM — whatever gates the repo defines, plus SPDX headers, reuse lint, no committed secrets or binaries, and that the PR targets staging. Use when the user says "check my work", "am I ready to open a PR", "run the pre-flight", "will CI pass", "did I break anything", "review my changes before I push", "/preflight", or is about to commit or push.
---

# check-my-contribution — the pre-flight, run for real

Run the gates. Report what failed and why. **Do not declare "looks good" without having
executed the commands.**

---

## 0 · Find out what this repo actually gates on

This repository is being built out, so **discover the gates rather than assuming them**:

```bash
ls -a
ls Makefile package.json go.mod .github/workflows/ 2>/dev/null
[ -f Makefile ] && make help
[ -f package.json ] && python3 -c "import json;print(json.load(open('package.json')).get('scripts',{}))"
[ -f .github/workflows/ci.yml ] && grep -n "name:\|run:\|working-directory:" .github/workflows/ci.yml
```

**Whatever the `Makefile` and the CI workflow say wins over this skill.** Run exactly what CI
runs — that's the only way "will CI pass?" gets an honest answer.

## 1 · What changed

```bash
git rev-parse --abbrev-ref HEAD
git status --short
git diff --stat origin/staging...HEAD 2>/dev/null || git diff --stat
```

## 2 · Run the gates

Based on what §0 found:

**If there's a Go module:**
```bash
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```
Use `-race` — the ecosystem's CI does, and a race that plain `go test` tolerates turns CI red.

**If there's a Node/TypeScript app:**
```bash
npm ci
npx tsc --noEmit
npm run lint
npm run test -- --run
npm run build
```

**Always:**
```bash
reuse lint          # pipx install reuse   (this ecosystem's CI pins 6.2.0)
```

If a gate can't run — no toolchain, no Docker, `reuse` not installed — report `SKIPPED — <reason>`
and say what CI will do with it. **Never report a gate as passing if you did not run it.**

## 3 · SPDX headers

A `reuse lint` gate is part of this ecosystem's CI: a file with no licensing information fails
the build.

Code in this repository is governed by **Community-Source (free for community members, fee for others; applied per work, by version notice — not per directory)**; documentation is
`LicenseRef-OpenLBM-Docs-1.0`. Check the repo's own `LICENSE-MAP.md` and `REUSE.toml` if they
exist — **they are authoritative over this skill**:

```bash
cat LICENSE-MAP.md REUSE.toml 2>/dev/null
```

Check every file you added:

<!-- REUSE-IgnoreStart -->

```bash
for f in $(git diff --name-only --diff-filter=A origin/staging...HEAD 2>/dev/null || git diff --name-only --diff-filter=A); do
  case "$f" in *.license) continue ;; esac
  printf '%-56s %s\n' "$f" "$(grep -m1 -o 'SPDX-License-Identifier: .*' "$f" 2>/dev/null || echo 'MISSING')"
done
```

<!-- REUSE-IgnoreEnd -->

Header formats: `//` for Go and TypeScript, `--` for SQL, an HTML comment for Markdown, `#`
comments **inside** YAML frontmatter for files that have any, and a `<filename>.license` sidecar
for anything that can't hold a comment.

Unsure which identifier applies? Use the **`licensing-check`** skill.

## 4 · Hygiene

```bash
# Build binaries and large files
git diff --cached --name-only | while read -r f; do
  [ -f "$f" ] && file "$f" | grep -q "executable\|ELF\|Mach-O" && echo "BINARY: $f"
done
git diff --name-only origin/staging...HEAD 2>/dev/null | while read -r f; do
  [ -f "$f" ] && du -h "$f"; done | sort -rh | head

# Key-shaped strings
git diff origin/staging...HEAD 2>/dev/null | \
  grep -nE 'sk-or-v1-|dop_v1_|ghp_|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----'

# .env files
git diff --name-only origin/staging...HEAD 2>/dev/null | grep -E '(^|/)\.env(\.|$)'
```

If a secret shows up, **do not just delete the line and commit again** — it stays in history.
Rotate the credential first, then rewrite the branch before pushing.

Also confirm you have not introduced `AUTH_MODE=dev` into any config that could reach a public
host. It is fine on demo/staging with fake data and never anywhere else.

## 5 · Convention checks

Where your change touches the shared data model or API, the ecosystem conventions apply. From
the `gable` repo's `CLAUDE.md`:

- [ ] **New DB columns**: UUID v4 PKs, `DECIMAL(19,4)` for physical quantities (never float),
      money-as-cents in application code, every quantity paired with a UOM id.
- [ ] **New migration**: additive and reversible; applies from empty.
- [ ] **New endpoint**: under the correct prefix and actually wired into the router — an
      endpoint that isn't registered silently doesn't exist.
- [ ] **New public path**: adding one deserves a security review.
- [ ] **Money formatting**: know whether the surface you're on speaks cents or dollars before
      you format anything. Getting it wrong renders `$738.87` as `$73,887.00`.
- [ ] **Financial operations** are audit-logged.

## 6 · The PR

- **Targets `staging`**, not `master`. Maintainers fast-forward `staging → master` after review.
  ```bash
  gh pr create --base staging --fill
  ```
- **Fill in the PR template honestly** if one exists (`ls .github/PULL_REQUEST_TEMPLATE.md`).
  Only tick a box you actually ran.
- **Commits are focused** — one logical change each.
- **The CLA.** Inbound contributions are licensed under the profile governing the files you
  touched; you'll be asked to agree before your first merge.

---

## Reporting back

```
Gate                    Result
gates discovered        Makefile + ci.yml present
go vet ./...            PASS
go test -race ./...     FAIL — see below
reuse lint              SKIPPED — reuse not installed
SPDX headers            PASS
secrets / binaries      clean
PR target               staging
```

Then, for each failure: the exact command, the exact error, and the smallest fix.
