# Retro Workflow

Capture learnings from completed work via parallel compounding.

## Important

- **NO rebasing** - unlike other workflows, do not rebase before starting
- **Two perspectives** - fresh subagent (context-free) + you (full context)

## Workflow

### 1. Get PR URL

```bash
PR_URL=$(gh pr view "$LINEAR_ISSUE_ID" --json url --jq '.url')
```

### 2. Launch Background Subagent (Parallel)

Use Bash tool with `run_in_background: true` to spawn a fresh subagent:

```bash
# run_in_background: true
claude -p "You are analyzing a completed PR to capture learnings.

Issue: $LINEAR_ISSUE_ID
PR: $PR_URL

1. Fetch the PR diff and description via gh pr view and gh pr diff
2. Invoke /compound-engineering:workflows:compound to document learnings
3. Write output to docs/solutions/ in the current directory

Focus on patterns that would help future implementations."
```

The subagent has NO prior context - it discovers what was learned from the PR alone.

### 3. Do Your Own Compound (In Parallel)

While the subagent runs in background, invoke `/compound-engineering:workflows:compound` yourself.

You have full implementation context - capture:
- What was hard
- What you would do differently
- What patterns emerged
- Decisions that weren't obvious from the code

Write to `docs/solutions/`.

### 4. Wait for Subagent

Check subagent completion before proceeding (you will be notified when background task completes).

### 5. Push

```bash
jj git push
```

### 6. Signal Completion

Add `worker-done` label to the Linear issue via MCP (see @references/linear-labels.md), then exit.
