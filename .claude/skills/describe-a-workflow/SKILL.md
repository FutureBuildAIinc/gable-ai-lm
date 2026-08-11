---
# SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
name: describe-a-workflow
description: Capture how a real lumber & building-materials job actually works — how estimators and counter staff actually work — as a structured spec with acceptance criteria an engineer can build from. Use when someone says "here's how we actually do this", "let me explain how it works at our yard", "this doesn't handle our process", "I want to describe a workflow", "can I contribute without coding", "I'm not a developer but I know how this should work", "spec out a feature", "/describe-workflow".
---

# describe-a-workflow — turn domain knowledge into something buildable

The scarcest input in this project is not code. It is someone who has actually done the job —
run a counter, priced a job, chased a delivery, reconciled a month of yard purchases — writing
down **exactly** how it works, including the messy and regional parts.

You are interviewing that person and producing a spec. They should never open a code editor.
**You are the scribe, not the architect.**

For **Gable AI_LM**, the workflows that matter most are how estimators and counter staff actually work: reading a plan set or a supplier PDF, turning a takeoff into a quote, catching a mis-priced line, and what a human must check before a machine-generated quote goes to a customer.

---

## 0 · Orient first

Find out what exists today so you can ask "what's different about yours?" rather than "how does
this work?".

```bash
ls -a                      # what is in this repo yet?
git clone --depth 1 https://github.com/FutureBuildAIinc/gable.git /tmp/gable
ls /tmp/gable/backend/internal/ /tmp/gable/app/src/pages/
sed -n '/Money convention/,/^###/p' /tmp/gable/CLAUDE.md
```

Two facts worth holding from the reference implementation:

- Orders flow `DRAFT → CONFIRMED → FULFILLED`, plus `ON_HOLD`. There is no will-call/pickup
  path and no separate picking step today.
- Fulfilling an order produces one tax-inclusive invoice plus a balanced
  `DR Accounts Receivable / CR Sales Revenue` GL entry and an AR subledger debit, in one
  transaction. Anything that creates or cancels a sale has to respect that.

Open by telling them what the system does today, in a sentence or two. Then ask what's
different.

## 1 · Interview

One question at a time, in plain language. Follow the tangents — that's where the requirements
live.

**The job.** What's it called here, and elsewhere? Who does it? What starts it? How do you know
it's done?

**The steps.** Walk me through it as if I'm shadowing you. What paperwork exists? Who signs
what? What does the customer see at each step? What gets printed, scanned, texted, or phoned?

**The reality.** What goes wrong most often? What's the workaround everyone uses? What must be
true before this can happen (credit, stock, truck, approval)? What happens when it isn't? Who
can override, and does anyone record it? What changes for a big commercial account versus a
small one? What changes at month-end or in the busy season?

**The numbers.** How many a day, and when? What does it cost when it goes wrong? What number
tells the owner it's going well?

**The edges.** Cancelled halfway? Customer changes their mind after step N? A partial? Different
in your province or state (tax, permits, hours of service)?

Ask explicitly at **every** step: does this move stock, create a charge, take a payment, or
change what the customer owes? Those steps need the most care.

## 2 · Write the spec

`docs/workflows/<workflow-name>.md`. Use this skeleton — it carries the house
acceptance-criteria triad.

<!-- REUSE-IgnoreStart -->

````markdown
<!--
SPDX-License-Identifier: LicenseRef-OpenLBM-Docs-1.0
SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors
-->

# Workflow: <Name>

**Contributed by:** <name / role / business type>
**Date:** <yyyy-mm-dd>
**Closest existing module:** <module in `gable`, or "none — greenfield">

## 1 · What this is, in one paragraph
Plain language. Define every local term you use.

## 2 · Who does it
| Role | What they do |
|---|---|

## 3 · Trigger and completion
- **Starts when:** …   - **Ends when:** …   - **Volume:** ~N/day, peaking <when>

## 4 · The happy path
Numbered. For each step: **who**, **what they see**, **what the system must record**, and
whether it **moves inventory**, **creates a charge**, or **takes a payment**.

## 5 · States
Every state and every legal transition — and the illegal ones, which become tests.

| From | Event | To | Notes |
|---|---|---|---|

## 6 · Rules and gates
Preconditions, what happens when one fails, who can override, what gets audit-logged.

## 7 · Data this needs
Fields in business terms. Flag every **money** field (tax-inclusive or not, what currency),
every **physical quantity** (name the unit of measure), and every **date/time** (timezone;
promise or record).

## 8 · What the customer sees
Notifications, documents, signatures, portal view. Quote wording where it matters legally.

## 9 · Edge cases
Each with an expected outcome, not a shrug.

## 10 · Reporting
The number that tells the owner this is working.

## 11 · Acceptance criteria

### Technical — what an automated test asserts
- [ ] <endpoint/input → status + shape; the full state matrix is table-tested>
- [ ] <illegal transition → 409, writes nothing (row count unchanged)>

### PRR (production readiness)
- [ ] **Auth + scoping** — who may call it; tenant/branch-scoped; does not rely on `AUTH_MODE=dev`
- [ ] **Money/data integrity + atomicity** — single writer, one transaction, no partial writes
- [ ] **Migrations additive + reversible**
- [ ] **No secrets committed**
- [ ] **Boot wiring** — registered and reachable; feature-flag/default state stated
- [ ] **Rollback plan**
- [ ] **Observability** — what is logged/measured; CI actually runs the new tests

### User-driven — the numbered walkthrough a real user runs to validate it
1. Navigate to … → expect …
2. Do … → expect …

## 12 · Open questions for maintainers
Where businesses differ, or where you genuinely don't know.

## 13 · How other systems do it
If you've run BisTrack, Spruce, or Agility: what did they call this, and what did they get
right or wrong? **Describe behaviour only — never paste their screens, schemas, API specs, or
documentation into this repository.**
````

<!-- REUSE-IgnoreEnd -->

## 3 · Quality bar

Ready when **all three** kinds of acceptance criteria are written and each is a **testable
statement**. Then:

- [ ] Someone with no industry background could follow §4 without asking a question.
- [ ] Every step that moves money or stock says so.
- [ ] Every state-transition row has an expected outcome, including the illegal ones.
- [ ] Every quantity names its unit of measure; every money field says tax-inclusive or not.
- [ ] The §11 walkthrough is numbered, concrete, and runnable.
- [ ] Open questions are listed, not silently decided.
- [ ] No competitor documentation, schemas, or API specs pasted in.

## 4 · File it

```bash
git fetch origin
git switch -c docs/workflow-<name> origin/staging
mkdir -p docs/workflows
git add docs/workflows/<name>.md
git commit -m "docs: capture <name> workflow spec"
```

> If `git switch` fails with `invalid reference: origin/staging`, this clone doesn't have
> `staging` yet — branch from the default branch and still open the PR **against `staging`**.

If the spec implies work in another product line, also open a feature request there and link
it. Then run **`check-my-contribution`**.

---

## Ground rules

- **You are the scribe.** Their process is the requirement. If a step looks wrong to you, put
  it in §12 as an open question — don't quietly redesign their business.
- **Never invent a step.** If nobody knows, write "unknown — needs a second interviewee".
- **Name the units and the tax basis.**
- **No competitor IP.** Describe behaviour from memory; paste nothing.
- **No real customer data.**
- **Regional differences are features, not noise.**
