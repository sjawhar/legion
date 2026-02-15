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

1. **Read Linear issue first** - `linear_linear(action="get", id="$LINEAR_ISSUE_ID")`
2. **Use jj, not git** - changes auto-tracked
3. **Signal completion** - add `worker-done` label when done (see routing table)
4. **Clean up on exit** - remove `worker-active` label when exiting (done or blocked)

## Skill Discipline

You are executing work with an approved plan. Do NOT invoke the brainstorming or writing-plans skills — your workflow has already been designed. Follow your assigned workflow file. The individual skills referenced in your workflow (TDD, subagent-driven-development, etc.) are appropriate to load and use.

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

When you need human input that the legion-oracle can't answer:

1. Push your work: `jj git push`
2. Post a structured escalation comment to Linear:

```
linear_linear(action="comment", id="$LINEAR_ISSUE_ID", body="
## Escalation

**Phase:** [current mode - architect/plan/implement/review]
**Completed:** [what work has been done so far]

### Blocker
[Specific question or decision needed — be precise]

### Options Considered
1. [Option A] — [trade-offs]
2. [Option B] — [trade-offs]
3. [Option C if applicable]

### Context
- **Remaining estimate:** [rough scope of remaining work after unblock]
- **Expertise needed:** [domain knowledge required to answer, e.g. 'product decision', 'API design', 'infrastructure']
- **Branch:** [current branch name if applicable]
")
```

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
| `merge` | @workflows/merge.md | No |

**Lifecycle order:** architect → plan → implement → review → (implement if changes requested) → retro → merge

**Retro** is not a mode — the controller resumes the implement worker's session with `/legion-retro`, preserving full implementation context. See the `legion-retro` skill.

## Review Mode Signaling

Review signals outcome via PR draft status BEFORE `worker-done`:
- **PR ready** (not draft) - no blocking issues, approved
- **PR draft** - blocking issues found, changes requested

## Research Before Escalating

Before blocking on user input, workers should invoke `/legion-oracle [your question]` to search institutional knowledge (docs/solutions/, codebase patterns). Only escalate to the user (via Linear comment + label) if the legion-oracle cannot answer.

## Reference

Label conventions: @references/linear-labels.md
