---
title: Skill vs State Machine Policy Boundary in Legion Controller
date: 2026-04-12
status: active
tags:
  - controller
  - state-machine
  - skill-authoring
  - policy
  - decision-engine
---

# Skill vs State Machine Policy Boundary in Legion Controller

## Problem

When a workflow behavior is wrong (e.g., retro being skipped, merge happening too early),
it's unclear whether to fix the TypeScript state machine (`decision.ts`) or the controller
skill (`SKILL.md`). Changing the wrong layer wastes time and may introduce regressions.

## The Boundary

**State machine (`decision.ts`) controls:**
- Status transitions (e.g., Needs Review → Retro → Done)
- Worker lifecycle signals (`worker-done`, `hasLiveWorker`)
- Merge gates (e.g., `IssueStatus.RETRO + worker-done → dispatch_merger`)
- CI gates and mergeability checks

**Controller skill (`SKILL.md`) controls:**
- Dispatch decisions and routing logic
- Skip conditions and bypass paths
- Prompt content sent to workers
- Human override handling
- Multi-step orchestration logic

## Diagnostic Pattern

When a phase is being skipped or bypassed incorrectly:

1. **Check the skill first.** Search for explicit skip/bypass logic in `SKILL.md`:
   ```bash
   grep -n "skip\|bypass\|directly\|without" .opencode/skills/legion-controller/SKILL.md
   ```

2. **Check the state machine second.** Verify the transition table in `decision.ts`
   returns the expected action for the issue's current state.

3. **The state machine is usually correct.** The TypeScript is tested (640+ tests) and
   rarely has policy bugs. Skill prose is untested and more likely to have incorrect
   bypass logic.

## Example: Retro Skip Bug (Issue #476)

The retro phase was being skipped. Investigation showed:

- **State machine**: Correct — `IssueStatus.RETRO + worker-done → dispatch_merger`
  already gated merge behind retro completion.
- **Controller skill**: Had explicit bash code that dispatched merge directly from
  Needs Review when `skipRetro=true` with no tricky parts/deviations, bypassing the
  state machine entirely.

**Fix**: Remove the skip logic from the skill. No state machine changes needed.

## When State Machine Changes ARE Needed

State machine changes are needed when:
- A new status or transition is required (e.g., adding a new pipeline phase)
- A new label signal needs to gate a transition
- The existing transition table produces the wrong action for a valid state combination

State machine changes are NOT needed when:
- The skill is dispatching workers out of order
- The skill has explicit bypass/skip logic that circumvents normal flow
- The skill is checking conditions the state machine already handles

## Related

- `packages/daemon/src/state/decision.ts` — the state machine
- `.opencode/skills/legion-controller/SKILL.md` — the controller skill
- `packages/daemon/src/state/__tests__/decision.test.ts` — state machine tests
