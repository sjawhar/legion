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

## GitHub PR Labels (Not Linear)

Review outcomes go on the **GitHub PR**, not Linear:

| Label | Meaning |
|-------|---------|
| `worker-approved` | PR passes review |
| `worker-changes-requested` | PR needs work |

```bash
gh pr edit --add-label <label>
```

**Critical ordering for reviewers:** Add PR label BEFORE `worker-done` on Linear.
