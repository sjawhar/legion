---
title: Removing a State Machine Action Type — Full Blast Radius
date: 2026-04-12
status: active
tags:
  - controller
  - state-machine
  - action-type
  - refactoring
  - delegation
---

# Removing a State Machine Action Type — Full Blast Radius

## Problem

When a state machine action type is wrong (e.g., `rebase_pr` — controller rebasing directly
instead of delegating to a worker), removing it requires updates across multiple files that
aren't all obvious from a single grep. Missing any one of them causes TypeScript errors or
silent behavioral regressions.

## Blast Radius Checklist

When removing or renaming an action type in `packages/daemon/src/state/`:

```
□ types.ts          — Remove from ActionType union
□ decision.ts       — Replace all return sites with the correct action
□ decision.ts       — Remove from ACTION_TO_MODE map
□ decision.test.ts  — Update all toBe("old_action") expectations
□ decision.test.ts  — Rename test descriptions to reflect new behavior
□ decision.test.ts  — Update any ACTION_TO_MODE[old_action] assertions
□ SKILL.md          — Action signal table (suggestedAction → controller should...)
□ SKILL.md          — Routing prefix table (prefix → intent → controller action)
□ SKILL.md          — Dedicated handler block (the "**`action_name`:**" prose section)
□ SKILL.md          — Any inline code examples that reference the action
□ SKILL.md          — Any condition tables that list the action as an outcome
```

**TypeScript as a guide:** After updating `types.ts` and `decision.ts`, run the package's typecheck
(`scripts/repo-tool.sh tsc --noEmit`, which the `typecheck` recipes run).
Compile errors will point to any remaining consumers. This catches `ACTION_TO_MODE` entries
and test assertions that reference the removed type.

## Example: `rebase_pr` → `resume_implementer_for_changes` (Issue #466)

The `rebase_pr` action had the controller calling GitHub's update-branch API directly.
This violated the delegation principle — rebase is worker work.

**Correct division:**
- Pre-approval conflicts → `resume_implementer_for_changes` (implementer rebases)
- Post-approval conflicts → merger worker handles rebase (already in merge.md)
- Controller → never rebases, never calls update-branch API

**Files changed:**

| File | Change |
|------|--------|
| `types.ts` | Removed `\| "rebase_pr"` from `ActionType` |
| `decision.ts` | Both `CONFLICTING` branches → `resume_implementer_for_changes` |
| `decision.ts` | Removed `rebase_pr: WorkerMode.REVIEW` from `ACTION_TO_MODE` |
| `decision.test.ts` | 4 `toBe("rebase_pr")` → `toBe("resume_implementer_for_changes")` |
| `decision.test.ts` | 1 `ACTION_TO_MODE.rebase_pr` test replaced |
| `decision.test.ts` | 1 `buildIssueState` test expectation updated |
| `SKILL.md` | 5 locations: signal table, prefix table, handler block, condition 7 table, retro-skip code |

## SKILL.md Has Multiple Independent References

A single action type typically appears in 3–5 distinct locations in `SKILL.md`:

1. **Signal table** — maps `suggestedAction` values to what the controller should do
2. **Routing prefix table** — maps action prefixes to intent
3. **Handler block** — prose section explaining the action in detail (e.g., `**\`rebase_pr\`:**`)
4. **Condition tables** — e.g., the Pre-Merge Gate condition 7 mergeability table
5. **Inline code examples** — bash snippets that check for or respond to the action

Search for all of them before declaring the skill updated:
```bash
grep -n "rebase_pr\|old_action_name" .opencode/skills/legion-controller/SKILL.md
```

## Merge Conflict Note

If a parallel PR also touches the same SKILL.md section (common when fixing related
boundary violations), the conflict resolution requires understanding the *intent* of both
changes — not just picking a side. In the #466 rebase, PR #478 had independently
strengthened the Role Boundary section and removed the retro-skip block. The correct
resolution was: take #478's stronger wording for `gh pr merge` + add #466's `jj git push`
line; take #478's unconditional retro rule (which superseded #466's retro-skip code change).

## Related

- `packages/daemon/src/state/types.ts` — ActionType union
- `packages/daemon/src/state/decision.ts` — suggestAction() and ACTION_TO_MODE
- `packages/daemon/src/state/__tests__/decision.test.ts` — decision tests
- `docs/solutions/controller/skill-vs-state-machine-policy-boundary.md` — when to fix skill vs state machine
