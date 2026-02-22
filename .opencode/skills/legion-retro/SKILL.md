---
name: legion-retro
description: Capture learnings from completed work via dual-perspective retrospective. Invoked by resuming an implement worker session — the implementer has full context, and a fresh subagent provides an outside view.
---

# Legion Retro

Capture learnings from completed work via parallel compounding.

## When This Runs

The controller resumes the **implement worker's existing session** after PR approval, so you (the implementer) have full context of what was built and why. This is intentional — your perspective as the person who did the work is valuable.

A fresh subagent provides the outside perspective (see step 2).

## Important

- **NO rebasing** - unlike other workflows, do not rebase before starting
- **Two perspectives** - fresh subagent (context-free) + you (full context)

## Workflow

### 1. Get PR URL

```bash
PR_URL=$(gh pr view "$LEGION_ISSUE_ID" --json url --jq '.url')
```

### 2. Launch Background Subagent (Parallel)

Use `background_task` tool to spawn a fresh subagent:

- **Category:** `unspecified-low`
- **Description:** "Retro analysis for $LEGION_ISSUE_ID"
- **Prompt:**

> You are analyzing a completed PR to capture learnings.
>
> Issue: $LEGION_ISSUE_ID
> PR: $PR_URL
>
> 1. Fetch the PR diff and description via gh pr view and gh pr diff
> 2. Invoke /compound-engineering/workflows/compound to document learnings
> 3. Write output to docs/solutions/ in the current directory
>
> Focus on patterns that would help future implementations.

### 3. Do Your Own Compound (In Parallel)

While the subagent runs in background, invoke `/compound-engineering/workflows/compound` yourself.

You have full implementation context - capture:
- What was hard
- What you would do differently
- What patterns emerged
- Decisions that weren't obvious from the code

Write to `docs/solutions/`.

### 4. Wait for Subagent

Check subagent completion before proceeding (you will be notified when background task completes).

### 5. Commit and Push Learnings

Ensure all `docs/solutions/` files are committed and pushed:

```bash
jj describe -m "$LEGION_ISSUE_ID: retro learnings"
jj git push
```

### 6. Post Summary to Issue

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

### 7. Signal Completion

Add `worker-done` label to the issue, then exit:

- **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current + "worker-done"])`
