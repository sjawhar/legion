---
name: legion-worker
description: Use when dispatched by Legion controller to work on a Linear issue in a jj workspace
---

# Legion Worker

Router skill for Legion issue work. Dispatched by controller with a mode parameter.

## Environment

Required:
- `LINEAR_ISSUE_ID` - issue identifier (e.g., `ENG-21`)
- `WORKSPACE_DIR` - jj workspace path
- `LEGION_DIR` - main Legion repo

## Essential Rules

1. **Read Linear issue first** - `mcp__linear__get_issue`
2. **Work in jj workspace** - all work in `$WORKSPACE_DIR`
3. **Use jj, not git** - changes auto-tracked
4. **Signal completion** - add `worker-done` label when done (see routing table)

## Mode Routing

| Mode | Workflow | Adds `worker-done` |
|------|----------|-------------------|
| `plan` | @workflows/plan.md | Yes |
| `implement` | @workflows/implement.md | No |
| `review` | @workflows/review.md | Yes |
| `retro` | @workflows/retro.md | Yes |
| `finish` | @workflows/finish.md | No |

**Lifecycle order:** plan → implement → review → (implement if changes requested) → retro → finish

Note: `retro` runs before `finish` because retro adds learnings to the workspace, and finish deletes it.

## Review Mode Labels

Review adds a GitHub PR label BEFORE `worker-done`:
- `worker-approved` - no blocking issues
- `worker-changes-requested` - blocking issues found

## Reference

Label conventions: @references/linear-labels.md
