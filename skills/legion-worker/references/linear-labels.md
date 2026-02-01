# Linear Labels

## The Only Linear Label

**`worker-done`** - Signals worker completion. Controller removes after processing.

### Adding via MCP

Preserve existing labels when adding:

```
1. mcp__linear__get_issue(id: ISSUE_ID) → extract labelIds array
2. mcp__linear__list_labels(teamId: TEAM_ID) → find "worker-done" ID
3. mcp__linear__update_issue(id: ISSUE_ID, labelIds: [...existing, workerDoneId])
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
