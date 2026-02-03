---
name: legion-worker
description: Use when dispatched by Legion controller to work on a Linear issue in a jj workspace
---

# Legion Worker

Router skill for Legion issue work. Dispatched by controller with a mode parameter.

## Environment

Required:
- `LINEAR_ISSUE_ID` - issue identifier (e.g., `ENG-21`)

## Essential Rules

1. **Read Linear issue first** - `mcp__linear__get_issue`
2. **Use jj, not git** - changes auto-tracked
3. **Signal completion** - add `worker-done` label when done (see routing table)
4. **Clean up on exit** - remove `worker-active` label when exiting (done or blocked)

## Session Lifecycle

### Starting

Sync with main and create a fresh commit on your branch:

```bash
jj git fetch
jj rebase -d main
jj new  # Fresh commit for this session
```

If you're resuming after user feedback, also read the Linear comments for the answer.

### Blocking on User Input

When you need human input that the oracle can't answer:

1. Push your work: `jj git push`
2. Post your question as a Linear comment: `mcp__linear__create_comment`
3. Update labels: add `user-input-needed`, remove `worker-active`
4. Exit immediately

The controller will resume your session when the user responds.

### Exiting

Always push before exiting:

```bash
jj git push
```

Then update labels:
- Add `worker-done` if your mode requires it (see routing table)
- Remove `worker-active` (the controller added this when dispatching you)

## Mode Routing

| Mode | Workflow | Adds `worker-done` |
|------|----------|-------------------|
| `architect` | @workflows/architect.md | Yes (or on children) |
| `plan` | @workflows/plan.md | Yes |
| `implement` | @workflows/implement.md | No |
| `review` | @workflows/review.md | Yes |
| `retro` | @workflows/retro.md | Yes |
| `merge` | @workflows/merge.md | No |

**Lifecycle order:** architect → plan → implement → review → (implement if changes requested) → retro → merge

Note: `retro` runs before `merge` because retro adds learnings to the workspace, and merge deletes it.

## Review Mode Signaling

Review signals outcome via PR draft status BEFORE `worker-done`:
- **PR ready** (not draft) - no blocking issues, approved
- **PR draft** - blocking issues found, changes requested

## Research Sub-Skill

Before blocking on user input, workers should invoke the oracle to try to find the answer:

| Sub-Skill | Workflow | Purpose |
|-----------|----------|---------|
| `oracle` | @workflows/oracle.md | Research before escalating |

Usage: `/oracle [your question]`

Only escalate to the user (via Linear comment + label) if the oracle cannot answer.

## Reference

Label conventions: @references/linear-labels.md
