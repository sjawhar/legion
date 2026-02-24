---
name: legion-retro
description: Capture learnings from completed work via retrospective. Invoked by resuming an implement worker session — the implementer has full context.
---

# Legion Retro

Capture learnings from completed work.

**CRITICAL: Do NOT use the Task tool to spawn subagents.** Subagents hang in headless serve
mode. Perform all analysis directly in this session.

## When This Runs

The controller resumes the **implement worker's existing session** after PR approval, so you (the implementer) have full context of what was built and why. This is intentional — your perspective as the person who did the work is valuable.

## Important

- **NO rebasing** - unlike other workflows, do not rebase before starting
- **Single session** - do all analysis yourself (no subagents)

## Workflow

### 1. Get PR URL and Diff

```bash
PR_URL=$(gh pr view "$LEGION_ISSUE_ID" --json url --jq '.url')
gh pr diff "$LEGION_ISSUE_ID"
```

### 2. Analyze and Document Learnings

Invoke `/compound-engineering/workflows/compound` to document learnings.

You have full implementation context — capture:
- What was hard
- What you would do differently
- What patterns emerged
- Decisions that weren't obvious from the code
- Patterns that would help future implementations

Write to `docs/solutions/`.

### 3. Commit and Push Learnings

Ensure all `docs/solutions/` files are committed and pushed:

**If VCS is `jj`:**
```bash
jj describe -m "$LEGION_ISSUE_ID: retro learnings"
jj git push
```

**If VCS is `git`:**
```bash
git add -A && git commit -m "$LEGION_ISSUE_ID: retro learnings"
git push
```

### 4. Post Summary to Issue

Post a brief summary to the issue so learnings are discoverable without checking the repo:

**GitHub:**

```bash
gh issue comment $ISSUE_NUMBER --body "## Retro Complete

**Learnings documented in:**
- [list docs/solutions/ files written]

**Key takeaways:**
- [1-3 bullet summary of the most important learnings]" -R $OWNER/$REPO
```

**Linear:**

```
linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="## Retro Complete

**Learnings documented in:**
- [list docs/solutions/ files written]

**Key takeaways:**
- [1-3 bullet summary of the most important learnings]")
```

### 5. Signal Completion

Add `worker-done` label to the issue, then exit:

- **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current + "worker-done"])`
