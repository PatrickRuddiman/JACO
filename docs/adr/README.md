---
sources:
  - docs/adr/
---

# Architecture Decision Records

This directory holds the load-bearing design decisions for JACO. An ADR is a
short document that says **what** was decided, **why**, and **what was
considered and rejected**. It is written *before* implementation and edited
only when the decision itself changes — not when the implementation evolves.

## When to write one

- Multi-PR efforts whose shape needs to outlive any single issue or PR.
- Decisions that constrain code in more than one package.
- Stance calls where future contributors will reasonably ask "why this way?"
  (security boundaries, isolation trade-offs, scheduler policy, data
  movement).

For trivial choices — naming, file layout inside a package, which standard
library helper to use — the answer is a code review comment, not an ADR.

## Format

One file per decision, named `NNNN-kebab-title.md`, numbered
monotonically. Each file:

```
# ADR NNNN: Title

- **Status:** proposed | accepted | superseded by ADR XXXX
- **Date:** YYYY-MM-DD
- **Issue:** #N (link to the GitHub issue that motivated it)

## Context
What problem are we solving and what constraints apply.

## Decision
What we are doing. In one or two paragraphs. Be specific.

## Alternatives considered
Each option, one paragraph, why rejected.

## Consequences
What this commits us to. Sequencing, follow-on work, things that get harder
because of this decision.
```

## How they get used

PRs that implement an ADR reference it in the description: `Implements ADR
0003`. Code that embodies a non-obvious decision links to the ADR in a doc
comment. When an ADR is superseded, the new one declares it and the old one's
status flips to `superseded by ADR XXXX` — but the file stays.

## Index

| ADR | Title | Status | Issue |
|---|---|---|---|
| [0002](0002-pressure-based-scheduling.md) | Pressure-based scheduling and migration | proposed | #92 |
| [0003](0003-orchestrator-comparison-benchmark.md) | Orchestrator comparison benchmark | proposed | #51 |

ADR 0001 (volume migration via stop-ship-start) was withdrawn during
design review; see #135 for the replacement direction (remote-mounted
volume backend).
