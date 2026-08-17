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
issue = linear_linear(action="get", id=ISSUE_ID)
current_labels = [label.name for label in issue.labels]

linear_linear(action="update",
  id=ISSUE_ID,
  labels=current_labels + ["worker-done"]
)
```

## GitHub PR Review State (Not Linear)

Review outcomes signaled via **native GitHub review API**, not labels:

| Review State | Meaning |
|-------------|---------|
| Approved | PR passes review |
| Changes Requested | PR needs work |

```bash
gh pr review "$LEGION_ISSUE_ID" --approve --body "..."           # Mark approved
gh pr review "$LEGION_ISSUE_ID" --request-changes --body "..."   # Mark changes requested
```

**Critical ordering for reviewers:** Submit review BEFORE `worker-done`.
