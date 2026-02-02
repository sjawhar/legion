# Linear Labels

## Worker Signal Labels

| Label | Meaning | Added by | Removed by |
|-------|---------|----------|------------|
| `worker-done` | Worker finished, controller should act | Worker | Controller |
| `user-input-needed` | Blocked on human input | Worker | Controller |
| `user-feedback-given` | Human answered | Human | Controller |

### Adding Labels via MCP

Labels array **replaces all labels**. Fetch current first:

```
issue = mcp__linear__get_issue(id: ISSUE_ID)
current_labels = [label.name for label in issue.labels]

mcp__linear__update_issue(
  id: ISSUE_ID,
  labels: current_labels + ["worker-done"]
)
```

## GitHub PR Draft Status (Not Linear)

Review outcomes signaled via **PR draft status**, not labels:

| Status | Meaning |
|--------|---------|
| Ready (not draft) | PR passes review |
| Draft | PR needs work |

```bash
gh pr ready "$LINEAR_ISSUE_ID"        # Mark approved
gh pr ready "$LINEAR_ISSUE_ID" --undo # Mark changes requested
```

**Critical ordering for reviewers:** Set draft status BEFORE `worker-done` on Linear.
