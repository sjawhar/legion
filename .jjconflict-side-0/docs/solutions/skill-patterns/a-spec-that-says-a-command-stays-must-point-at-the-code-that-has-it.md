---
title: "A spec that says a command or surface 'stays' must point at the code that has it: the legion approve phantom a design note carried into the LEGION-20 spec"
category: skill-patterns
tags:
  - spec-writing
  - planning
  - phantom-surface
  - verify-before-planning
  - design-notes
date: 2026-09-13
status: active
module: skills/legion-architect, skills/legion-worker
related_issues:
  - "LEGION-20"
  - "sjawhar/legion#975"
---

# A spec that says a command or surface "stays" must point at the code that has it

## What happened

The LEGION-20 spec inherited, from an earlier design note
(`docs/superpowers/specs/2026-09-10-legion-dispatch-lifecycle-design.md`, the line listing
`legion admit`/`legion backlog`/`legion approve` as a CLI vocabulary), the statement that an
operator command `legion approve <issue>` exists and *stays* as the non-human way past the
design gate. The planner's grep of `packages/daemon/src/cli/index.ts` found no such command: it
was a proposal in the note that the shipped lifecycle never implemented. Had the plan trusted the
spec, the branch would have "kept" a command by writing it — inventing an operator bypass for a
human gate that the shipped design deliberately does not have (`gates.design: off` in the daemon
config is the only bypass; the root `AGENTS.md` says so).

## The rule

A design note is a snapshot of an intention, not a description of the code. Whenever a spec or
plan says a surface *exists*, *stays*, *is unchanged*, or *is reused* — a CLI command, a tool op, a
route, a config key, an event type — the author names where it lives (file and symbol) and the
reader greps for it before planning around it. The check is one command:

```sh
rg -n 'approve' packages/daemon/src/cli/index.ts skills/ packages/pi-envoy/src/legion/tools.ts
```

If the grep finds nothing, the sentence is a proposal. Either the spec drops it (the LEGION-20
outcome: "There is no operator command to open a gate and no agent path either") or it becomes an
explicit new-work item with its own acceptance line — never a silent "stays".

The same discipline caught the other direction on this issue: the spec's claim that the daemon
API contract number covers a `register_gate` shape change was true in the code
(`LEGION_DAEMON_API_VERSION`), but the plan had not scheduled the bump; a grep of the contract
file is what surfaced it. Both are the same move — verify a claimed-existing surface against the
tree before treating it as settled.

## Related

- `docs/solutions/skill-patterns/plan-ruling-that-changes-a-user-decision-goes-back-to-the-user.md`
  — the neighbouring failure mode, a plan that quietly changes a decision.
- `docs/solutions/daemon/plugin-daemon-api-contract-version-gate.md` — the contract bump the
  plan missed.
