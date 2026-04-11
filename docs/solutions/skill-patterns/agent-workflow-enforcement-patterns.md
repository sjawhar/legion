---
title: "Enforcing Invariants in Agent Workflow Markdown"
category: skill-patterns
tags:
  - skills
  - workflows
  - enforcement
  - handoff
  - failure-modes
  - cross-cutting
date: 2026-04-11
status: active
module: worker
related_issues:
  - "430"
  - "431"
symptoms:
  - "workers signal worker-done without writing handoff data"
  - "handoff pipeline has 50% miss rate"
  - "echo ERROR in workflow markdown does not stop agent execution"
---

# Enforcing Invariants in Agent Workflow Markdown

## Problem

Agent workflows are markdown files that agents read and interpret — they are prose with embedded shell examples, not executable scripts. Traditional enforcement mechanisms (`exit 1`, exceptions, return codes) are unavailable or counterproductive in this environment.

Specifically: `exit 1` in a markdown code block kills the agent's entire shell session, preventing cleanup (pushing changes, updating labels, sending notifications). This makes "fail fast" patterns impossible without worse consequences than the original failure.

## Pattern: Layered Social Enforcement

When an invariant must hold (e.g., "handoff file must exist before signaling completion"), use three independent layers that compound enforcement probability:

### Layer 1: FATAL Verification Blocks (Per-Step)

Place a verification block immediately after the action that should produce the invariant:

```bash
if [ ! -f .legion/<phase>.json ]; then
  echo "FATAL: Handoff write failed — .legion/<phase>.json not created"
  echo "STOP: Do NOT signal worker-done. Diagnose: Is 'legion' CLI in PATH? Is --workspace correct?"
  echo "If write cannot be fixed, note the failure in your exit comment."
fi
```

**Why this works:** Agents pattern-match on severity keywords. `FATAL` + explicit `STOP: Do NOT...` directives are interpreted as high-priority instructions, unlike `ERROR` which agents may log and continue past.

**Why `ERROR` fails:** In the original implementation, `echo "ERROR: ..."` was treated as informational output. Agents read it, noted it, and continued to signal `worker-done` anyway. The word "ERROR" doesn't carry the same behavioral weight as "FATAL" + "STOP" in agent instruction parsing.

### Layer 2: Tracked Todo Obligation (Session-Level)

Add a dedicated todo item in the worker's startup todos:

```
Write handoff data: complete the workflow handoff write and verify .legion/$MODE.json exists before signaling completion
```

This creates a persistent tracking obligation. Even if the agent skips the verification block, the pending todo reminds it that handoff hasn't been completed.

### Layer 3: Pre-Exit Gate (Cross-Cutting)

Add a verification check in the shared exit sequence (e.g., `legion-worker/SKILL.md` Exiting section) that fires for all modes that require handoff:

```bash
if printf '%s\n' architect plan implement test review | grep -qx "$MODE"; then
  if [ ! -f ".legion/${MODE}.json" ]; then
    echo "FATAL: Missing required handoff file .legion/${MODE}.json"
    echo "STOP: Do NOT push or signal worker-done."
  fi
fi
```

This catches agents that completed the workflow but missed the per-step check.

## Gotcha: Mode Exclusion List

Not all modes write handoff data. `merge` mode does not produce a `.legion/merge.json`. The exit gate must explicitly list modes that require handoff files. If a new mode is added that writes handoff data, it must be added to this list.

The allowlist (`architect plan implement test review`) is maintained in the exit gate. Future mode additions must consciously decide whether they write handoff data and update accordingly.

## Gotcha: Severity Word Choice Matters

Agents respond differently to different severity words in markdown instructions:

| Word | Agent Behavior |
|------|---------------|
| `ERROR` | Often treated as informational; agent logs and continues |
| `FATAL` | Treated as a stop condition; agent pauses to assess |
| `STOP: Do NOT...` | Explicit behavioral directive; agent follows instruction |

The combination of `FATAL` + `STOP: Do NOT signal worker-done` is the strongest available signal. Using only `FATAL` without the explicit directive is weaker.

## Gotcha: Effort-Based vs. Outcome-Based Requirements

Weak: "You MUST **attempt** the handoff write before signaling completion."
Strong: "You MUST **complete** the handoff write and **confirm** `.legion/$MODE.json` exists before signaling completion."

Effort-based requirements ("attempt") allow agents to satisfy the obligation without achieving the outcome. Outcome-based requirements ("complete and confirm") tie the obligation to a verifiable state.

## Validation

Applied to issue #430. Branch audit showed ~50% handoff miss rate before the fix (5 of 10 sampled branches had `.legion/` data). The three-layer approach was chosen because any single layer can be missed by an agent, but the probability of missing all three is much lower.
