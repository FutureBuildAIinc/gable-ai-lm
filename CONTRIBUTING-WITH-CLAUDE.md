<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# Contributing to Gable AI_LM with Claude Code

> ## Read this first
>
> This repository holds the AI_LM service: a Go backend under `backend/`, a Lit 3 frontend
> under `app/`, and its own Postgres schema. The migration out of the older `futurebuildai`
> organisation is **complete** — this is where the behaviour is. A stale copy may still exist
> elsewhere on a contributor's disk; it is not authoritative.
>
> The Claude Code kit is installed *ahead* of the migration on purpose, so the conventions
> arrive with the first commit rather than being retrofitted afterwards. Nothing below is
> aspirational about the kit itself — the skills are real and they work now. What's aspirational
> is the code they'll eventually be pointed at.

**Gable AI_LM** is the AI satellite for lumber & building materials: document and takeoff
parsing, quoting assistance, and the AI-driven surfaces that sit alongside the core ERP.

Which means the most valuable thing you can contribute **right now** is not code. It's a written
description of how estimators and counter staff actually work — reading a plan set or a supplier
PDF, turning a takeoff into a quote, catching a mis-priced line, and **what a human must check
before a machine-generated quote goes to a customer.** That last one is the specification this
product line most needs and least has.

---

## What Claude Code is

Claude Code is a command-line tool from Anthropic. You point it at a folder, describe what you
want in ordinary English, and it reads the files, runs commands, and makes changes — showing you
each step and asking before it does anything significant.

It is not magic and it is not always right. That matters more here than anywhere else in the
ecosystem: this is a product line about machine-generated numbers reaching customers, being
built with machine assistance. Think of Claude as a fast, tireless assistant who has never had
to explain a wrong quote to a builder. You supply the judgement.

### Install it

1. **Get the repository onto your computer.** You need [Git](https://git-scm.com/downloads).
   ```bash
   git clone https://github.com/FutureBuildAIinc/gable-ai-lm.git
   cd gable-ai-lm
   ```
2. **Install Claude Code.** Follow the official instructions at
   <https://docs.claude.com/en/docs/claude-code/overview>. You will need a Claude account.
3. **Start it, from inside the `gable-ai-lm` folder:**
   ```bash
   claude
   ```

That's it. Type what you want in plain English and press enter.

### The kit loads itself

This repository ships a `.claude/` folder containing everything below — the skills, the
slash-command shortcuts, and a `settings.json` that pre-approves the Go and Node toolchain
commands the gates need (`go build`, `go vet`, `go test`, `gofmt`, `npx tsc --noEmit`,
`npm run lint`, `npm run test`, `npm run build`, `npm ci`), plus `reuse lint` and read-only
`git`, while blocking reads of `.env` files. When you start Claude Code from inside
`gable-ai-lm`, it picks all of that up automatically.

You don't install anything. You don't configure anything. It's already there.

Because the repository is empty, the skills are written to **discover** rather than assume: they
check for a `Makefile`, a `package.json`, a `go.mod`, and a CI workflow, and run whatever is
actually there. Whatever the `Makefile` and CI say **wins over the skill**. That's the design —
it means the kit won't go stale the day the code lands.

---

## The skills

A **skill** is a set of instructions Claude follows for a particular kind of job. You never have
to name one — describe what you want and the right skill activates. Each also has a **slash
command** shortcut if you prefer.

| Skill | Shortcut | Use it when |
|---|---|---|
| **describe-a-workflow** | `/describe-workflow` | You know how estimating and quoting really work — plan sets, takeoffs, supplier PDFs, catching a mis-priced line, what a human signs off before a quote leaves. **The highest-value contribution to this repository today, and it requires no coding.** |
| **report-an-issue** | `/file-issue` | Something behaved wrong. Claude's first job is **routing** — with no code here, it will help you file where it can actually be acted on. |
| **improve-docs** | `/fix-doc` | A doc is wrong or missing. Creating one is a contribution, but only if you can verify what you write. |
| **licensing-check** | `/license-of` | "Which licence covers this?", "Do I have to pay?", "Can my company use it?" This repository is **Community-Source**, which behaves differently from the rest — see below. |
| **check-my-contribution** | `/preflight` | You're about to open a pull request. Discovers the gates, runs exactly what CI runs, then SPDX, `reuse lint`, secrets, and PR target. |

---

## Two worked examples

### 1 · You're an estimator and nobody has written down what you actually check

This is the contribution the repository needs most, and you will never open a code editor.

**Type this:**

> I do takeoffs from PDF plan sets. Nothing automated has ever got my openings right, and there
> are four things I check on every quote before it goes out. Let me walk you through it.

**What happens:** Claude runs the `describe-a-workflow` skill and becomes your scribe. It will:

- **Orient itself first** against the reference implementation so it can ask *"what's different
  about yours?"* rather than *"how does quoting work?"*.
- **Interview you one question at a time**, following the tangents. What starts it? What does the
  paperwork look like? What goes wrong most often, and what's the workaround everyone uses? Who
  can override, and does anyone record that? What changes for a big commercial account versus a
  cash walk-in? What changes in the busy season?
- Ask at **every** step: does this move stock, create a charge, take a payment, or change what
  the customer owes?
- Press hardest on the review gate — **what must a human confirm before a machine-generated
  number reaches a customer, and what gets audit-logged when they do.** In this product line
  that isn't a nice-to-have, it's the production-readiness bar.
- Write it up with **three** kinds of acceptance criteria: technical (what an automated test
  asserts, including the *illegal* transitions), production-readiness (auth and scoping,
  atomicity, reversible migrations, rollback plan, observability), and a **numbered walkthrough a
  real estimator runs** to confirm it was built right.
- Name the unit of measure on every quantity and the tax basis on every money field. *"500 board
  feet, tax-exclusive"* is a spec; *"the total"* isn't.
- Put what you don't know in §12 as an open question rather than silently deciding it.

Your process is the requirement. If Claude thinks a step is wrong, it goes in the open-questions
section — **it does not quietly redesign your job.**

### 2 · You hit a bug in an AI feature today

**Type this:**

> The document parser returned a 202 and then nothing ever appeared. Is it broken?

**What happens:** the `report-an-issue` skill, and the first two things it does are the important
ones.

**It checks whether this is a security problem** — seeing another customer's data, reaching
something without logging in, a credential appearing anywhere. Those never go in a public issue.

**Then it checks the known landmines before letting you file**, several of which look like bugs
and aren't:

- **Image and document generation is asynchronous.** It returns `202` with a `generating` status
  and finalises in the background while the client polls. Synchronous generation times out behind
  a gateway, which is why it works this way.
- **AI features degrade gracefully by design.** Keys are resolved at runtime — DB-first via
  settings, environment fallback — and a **missing key means the feature is unavailable, not that
  the app fails.** "The AI thing didn't do anything" is often a missing key, not a defect.
- **`AUTH_MODE=dev` is the standing production-readiness blocker** for any real deploy of this
  product line. Fine on demo/staging with fake data; never anywhere else.
- **Anything touching money or a customer-facing quote is a review gate** — a machine-generated
  number reaching a customer needs a human in the loop and an audit trail.

**Then it routes.** Since this repository has no code, Claude will say so plainly and help you
file where the behaviour actually lives rather than opening an issue nobody can act on.

---

## The ground rules

These apply whether or not you used AI — and they apply from the first commit, which is the point
of installing the kit early.

**1 · Pull requests target `main`.**
`main` is the only branch today — see `CONTRIBUTING.md`. Branch as `<line>/<slug>` and open the
PR against `main`.

**2 · Never commit a secret.**
This one is sharper here than elsewhere, because this product line is the one with API keys in
it. No keys, tokens, passwords, connection strings, or `.env` files. If one lands in a commit,
deleting it later doesn't help — it stays in history. **Rotate the credential first**, then
rewrite the branch before pushing. The kit's `.claude/settings.json` blocks reading `.env` files
for exactly this reason, and model-provider keys belong in runtime settings, never in code.

Related: never introduce `AUTH_MODE=dev` into any config that could reach a public host.

**3 · The SPDX header must match the directory.**
**Documentation and everything under `.claude/` is `LicenseRef-OpenLBM-Docs-1.0`**; the licence
text is in [`LICENSES/`](./LICENSES/). **Code** in this repository is governed by
**Community-Source** — free for community members, a fee for others — and it is the one Profile
in the Standard that is **applied per work, by a version notice, not per directory.** Don't
reason about it the way you'd reason about the directory-scoped Profiles.

When code lands here it will bring a `LICENSE-MAP.md` and a `REUSE.toml` with it, and **those are
authoritative over any skill or over this document.** Order of authority: the file's own
`SPDX-License-Identifier` → `REUSE.toml` → `LICENSE-MAP.md`. Until then, the canonical Standard
is at <https://github.com/FutureBuildAIinc/openlbm> and the reference per-component map is
[`LICENSE-MAP.md` in `FutureBuildAIinc/gable`](https://github.com/FutureBuildAIinc/gable/blob/master/LICENSE-MAP.md).

Header formats: `//` for Go and TypeScript, `--` for SQL, an HTML comment for Markdown, `#`
comments **inside** YAML frontmatter for files that have any, and a `<filename>.license` sidecar
for anything that can't hold a comment. Ask with `/license-of <path>` if unsure, and run
`reuse lint` before you push.

**4 · Security problems go through the private channel, not a public issue.**
This repository has no `SECURITY.md` of its own yet, so follow
[the policy in `FutureBuildAIinc/gable`](https://github.com/FutureBuildAIinc/gable/blob/master/SECURITY.md):
the **Security** tab → **Report a vulnerability**, or **security@futurebuild.ai**.

**5 · Never paste in competitor material.**
Describe how BisTrack, Spruce, or Agility behave from memory if it's useful. Do not paste their
documentation, schemas, screenshots, or API specifications into this repository. The same goes
for a customer's plan set or supplier price file — those are somebody's confidential documents,
and they are exactly the kind of thing that gets pasted into an AI repository by accident.

**6 · No real customer data** in examples, specs, issues, screenshots, or test fixtures.

**7 · Run `/preflight` before you push**, and keep it to one concern per PR.

**8 · Contributions are licensed under the Profile governing the files you touched, via a CLA.**
You'll be asked to agree before your first merge.

---

## An honest note about AI-assisted contributions

**AI-assisted contributions are welcome here.** We built this kit on purpose. We would rather
have an estimator's real process filtered through an AI assistant than not have it at all.

But there's a bargain, and it isn't negotiable:

> **You are responsible for what you submit.** Your name goes on the pull request. When a
> reviewer asks "why does this work?", "AI wrote it" is not an answer.

This repository is where that principle gets tested twice over. The product itself is built on
the premise that a machine-generated number needs a human check before it reaches a customer.
The same premise applies to the code that produces it. If you wouldn't let an unreviewed
machine-generated quote go to a builder, don't let an unreviewed machine-generated diff go to
`staging`.

There's an extra trap while this repository is empty: **an AI asked about a codebase that doesn't
exist will happily describe one.** If Claude tells you about a file, an endpoint, or a screen in
`gable-ai-lm`, check that it exists before you believe it. The skills are written to run `ls -a`
first and say plainly when there's nothing here — but you are the backstop.

Concretely, before you open a PR:

- **Read every line of the diff.** If you don't understand a change, ask Claude to explain it
  until you do — or drop it.
- **Run it yourself.** Run `/preflight`. Actually start the thing and click it.
- **Check the facts.** Claude can state a wrong file path or a plausible-sounding command with
  complete confidence. If a command in a skill doesn't exist, that's a bug — file it with
  `/file-issue`.
- **Be sceptical of anything that produces a number.** A confident wrong price is the
  characteristic failure of this product line, and it's also the characteristic failure of a
  language model. Check the arithmetic yourself.
- **Say that you used AI** in the PR description. Nobody minds. It helps reviewers know where to
  look hardest.

And the flip side: **a workflow spec is a complete contribution.** You do not have to submit
code. Right now, in this repository, it's worth considerably more than code would be.

---

## Where to go next

| You want to… | Go to |
|---|---|
| Build and run Gable locally | [`FutureBuildAIinc/gable`](https://github.com/FutureBuildAIinc/gable) → `README.md` |
| Understand the conventions, the money boundary, the gotchas | [`FutureBuildAIinc/gable`](https://github.com/FutureBuildAIinc/gable) → `CLAUDE.md` |
| Understand the branch model, PR workflow, CLA | [`FutureBuildAIinc/gable`](https://github.com/FutureBuildAIinc/gable) → `CONTRIBUTING.md` |
| Read the architecture and reference docs | [`FutureBuildAIinc/gable-docs`](https://github.com/FutureBuildAIinc/gable-docs) |
| Understand the licensing Standard, and Community-Source in particular | <https://github.com/FutureBuildAIinc/openlbm> |
| Build an installable app against the plug-in seam | [`FutureBuildAIinc/gable-sdk`](https://github.com/FutureBuildAIinc/gable-sdk) |
| Use the Gable name or logo | [`FutureBuildAIinc/brand`](https://github.com/FutureBuildAIinc/brand) |
| Report a security problem | [`gable/SECURITY.md`](https://github.com/FutureBuildAIinc/gable/blob/master/SECURITY.md) |
