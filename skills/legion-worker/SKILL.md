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

## Mode Routing

| Mode | Workflow | Adds `worker-done` |
|------|----------|-------------------|
| `architect` | @workflows/architect.md | Yes (or on children) |
| `plan` | @workflows/plan.md | Yes |
| `implement` | @workflows/implement.md | No |
| `review` | @workflows/review.md | Yes |
| `retro` | @workflows/retro.md | Yes |
| `finish` | @workflows/finish.md | No |

**Lifecycle order:** architect → plan → implement → review → (implement if changes requested) → retro → finish

Note: `retro` runs before `finish` because retro adds learnings to the workspace, and finish deletes it.

## Review Mode Signaling

Review signals outcome via PR draft status BEFORE `worker-done`:
- **PR ready** (not draft) - no blocking issues, approved
- **PR draft** - blocking issues found, changes requested

## Research Sub-Skill

Before using `AskUserQuestion`, workers should invoke the oracle:

| Sub-Skill | Workflow | Purpose |
|-----------|----------|---------|
| `oracle` | @workflows/oracle.md | Research before escalating |

Usage: `/oracle [your question]`

## Reference

Label conventions: @references/linear-labels.md
