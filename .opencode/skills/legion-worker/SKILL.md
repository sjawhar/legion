---
name: legion-worker
description: Use when dispatched by Legion controller to work on an issue in a git workspace
---

# Legion Worker

Router skill for Legion issue work. Dispatched by controller with a mode parameter.

## Context from Prompt

The controller dispatches you with a prompt that includes your **issue ID**, **mode**, **backend**, and **VCS**:

- **GitHub:** `/legion-worker implement mode for acme-widgets-42 (github backend, repo: acme/widgets, vcs: git)`
- **Linear:** `/legion-worker plan mode for ENG-21 (linear backend, vcs: jj)`

Extract these values from the prompt. For GitHub issues, also derive the **owner**, **repo**,
and **issue number** from the issue ID (format: `owner-repo-number`). The **VCS** value (`jj` or `git`) determines which version control commands to use.

Throughout this skill and its workflows, `$LEGION_ISSUE_ID`, `$ISSUE_NUMBER`, `$OWNER`, and
`$REPO` are **placeholders** — substitute the values you extracted from your prompt context.
Use the **backend** from your prompt to choose GitHub CLI or Linear MCP commands.

## Essential Rules

1. **Read issue first**
   - **GitHub:** `gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO`
   - **Linear:** `linear_linear(action="get", id="$LEGION_ISSUE_ID")`
2. **Use the VCS from your prompt** (`jj` or `git`) for version control
3. **Signal completion** - add `worker-done` label when done (see routing table)
4. **Clean up on exit** - remove `worker-active` label when exiting (done or blocked)

## Skill Discipline

You are executing work with an approved plan. Do NOT invoke the brainstorming or writing-plans skills — your workflow has already been designed. Follow your assigned workflow file. The individual skills referenced in your workflow (TDD, subagent-driven-development, etc.) are appropriate to load and use.

## CRITICAL: No Subagents

**Do NOT use the Task tool to spawn subagents.** Subagent sessions hang in headless serve mode.
Perform all code searches (grep, glob, read) and analysis directly in this session. Do not
delegate work to explore agents, research agents, or any other subagent type.

## Session Lifecycle

### Starting

Ensure you're on the right branch and synced with main:

**If VCS is `jj`:**
```bash
jj git fetch
jj rebase -d main
```

**If VCS is `git`:**
```bash
git fetch origin
git checkout -b legion/$LEGION_ISSUE_ID origin/main 2>/dev/null || git checkout legion/$LEGION_ISSUE_ID
git pull --rebase origin main
```

If you're resuming after user feedback, also read the issue comments for the answer.

### Blocking on User Input

When you need human input that the legion-oracle can't answer:

1. Push your work:
   - **jj:** `jj git push`
   - **git:** `git push -u origin HEAD`
2. Post a structured escalation comment to the issue:

**GitHub:**
```
gh issue comment $ISSUE_NUMBER --body "## Escalation

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
- **Branch:** [current branch name if applicable]" -R $OWNER/$REPO
```

**Linear:**
```
linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="## Escalation

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
- **Branch:** [current branch name if applicable]")
```

3. Update labels: add `user-input-needed`, remove `worker-active`
4. Exit immediately

The controller will resume your session when the user responds.

### Exiting

Always commit and push before exiting:

**If VCS is `jj`:**
```bash
jj describe -m "your commit message"
jj git push
```

**If VCS is `git`:**
```bash
git add -A && git commit -m "your commit message"
git push -u origin HEAD
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

Before blocking on user input, workers should invoke `/legion-oracle [your question]` to search institutional knowledge (docs/solutions/, codebase patterns). Only escalate to the user (via issue comment + label) if the legion-oracle cannot answer.

## Reference

Label conventions: @references/linear-labels.md (Linear), @references/github-labels.md (GitHub)
