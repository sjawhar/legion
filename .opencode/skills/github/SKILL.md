---
name: github
description: Manage GitHub issues via Projects V2. Use when LEGION_ISSUE_BACKEND=github.
---

# GitHub (gh CLI)

Direct CLI operations via `gh`. No embedded MCP — all commands are shell invocations.

## Setup

Requires `gh` CLI installed and authenticated:
```bash
gh auth login
```

## Operations

### Search/List Issues (via project)

List all items in a GitHub Project V2:

```bash
gh project item-list $PROJECT_NUM --owner $OWNER --format json
```

**Parameters:**
- `$PROJECT_NUM`: Project number (from `LEGION_ID` format: `owner/project-number`)
- `$OWNER`: Repository owner
- `--format json`: Returns structured data for parsing

**Example:**
```bash
gh project item-list 42 --owner acme --format json | jq '.items[] | {id, title, status}'
```

### Get Issue Details

Fetch full issue metadata:

```bash
gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO
```

**Parameters:**
- `$ISSUE_NUMBER`: Issue number (e.g., `123`)
- `-R $OWNER/$REPO`: Repository (required for multi-repo support)
- `--json`: Fields to return (title, body, labels, comments, state, etc.)

**Example:**
```bash
gh issue view 123 --json title,body,labels,state -R acme/backend
```

### Update Status (Projects V2 — GraphQL)

Update issue status in a GitHub Project V2. Requires field and option IDs from project schema:

```bash
gh api graphql -f query='mutation {
  updateProjectV2ItemFieldValue(input: {
    projectId: "$PROJECT_ID"
    itemId: "$ITEM_ID"
    fieldId: "$STATUS_FIELD_ID"
    value: { singleSelectOptionId: "$OPTION_ID" }
  }) { projectV2Item { id } }
}'
```

**Parameters:**
- `$PROJECT_ID`: GraphQL ID of the project (not the number)
- `$ITEM_ID`: GraphQL ID of the issue in the project
- `$STATUS_FIELD_ID`: GraphQL ID of the Status field
- `$OPTION_ID`: GraphQL ID of the status option (e.g., "In Progress", "Done")

**Note:** Field and option IDs must be resolved from the project schema. The controller caches these after first query.

**Resolve IDs (one-time):**
```bash
gh api graphql -f query='query {
  repository(owner: "$OWNER", name: "$REPO") {
    projectV2(number: $PROJECT_NUM) {
      fields(first: 20) {
        nodes {
          ... on ProjectV2SingleSelectField {
            id
            name
            options { id name }
          }
        }
      }
    }
  }
}'
```

### Add Label

Add a label to an issue (additive — does not remove existing labels):

```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO
```

**Parameters:**
- `$ISSUE_NUMBER`: Issue number
- `--add-label`: Label to add (can be used multiple times)
- `-R $OWNER/$REPO`: Repository

**Example:**
```bash
gh issue edit 123 --add-label "worker-done" --add-label "reviewed" -R acme/backend
```

### Remove Label

Remove a label from an issue:

```bash
gh issue edit $ISSUE_NUMBER --remove-label "worker-active" -R $OWNER/$REPO
```

**Parameters:**
- `$ISSUE_NUMBER`: Issue number
- `--remove-label`: Label to remove (can be used multiple times)
- `-R $OWNER/$REPO`: Repository

**Example:**
```bash
gh issue edit 123 --remove-label "worker-active" -R acme/backend
```

### Comment on Issue

Add a comment to an issue:

```bash
gh issue comment $ISSUE_NUMBER --body "Fixed in commit abc123" -R $OWNER/$REPO
```

**Parameters:**
- `$ISSUE_NUMBER`: Issue number
- `--body`: Comment text (supports Markdown)
- `-R $OWNER/$REPO`: Repository

**Example:**
```bash
gh issue comment 123 --body "Implemented in PR #456" -R acme/backend
```

### Create Issue

Create a new issue:

```bash
gh issue create --title "Bug: Login fails" --body "Details" -R $OWNER/$REPO
```

**Parameters:**
- `--title`: Issue title (required)
- `--body`: Issue description (optional, supports Markdown)
- `-R $OWNER/$REPO`: Repository

**Example:**
```bash
gh issue create --title "Feature: Add dark mode" --body "User request from #789" -R acme/backend
```

## Key Differences from Linear

| Aspect | Linear | GitHub |
|--------|--------|--------|
| **Labels** | Replace all (read-modify-write) | Additive (`--add-label`, `--remove-label`) |
| **Status** | Direct field update | Projects V2 GraphQL mutation |
| **PR Association** | Attachment field | Native (issue ↔ PR link) |
| **API** | MCP tool dispatch | Direct `gh` CLI |
| **Multi-repo** | Single team | `-R owner/repo` per command |

## Important Notes

- **Always specify `-R $OWNER/$REPO`** for multi-repo project support
- **Labels are additive**: Use `--add-label` and `--remove-label` separately (unlike Linear which replaces all)
- **Status updates require Projects V2 GraphQL** — not just issue labels
- **PR association is automatic** — GitHub links issues and PRs natively
- **`$OWNER` and `$REPO` come from `LEGION_ID`** (format: `owner/project-number`)
- **Field/option IDs must be cached** by the controller after first resolution

## Error Handling

Common errors and solutions:

| Error | Cause | Solution |
|-------|-------|----------|
| `Could not resolve to a Repository` | Wrong `-R` format | Use `-R owner/repo` (not `owner-repo`) |
| `Could not resolve to an Issue` | Issue doesn't exist | Verify issue number is correct |
| `GraphQL error: Field not found` | Wrong field ID | Re-resolve field IDs from project schema |
| `Not authenticated` | `gh` not logged in | Run `gh auth login` |

## Reference

- **Project number**: Visible in GitHub UI (e.g., `https://github.com/orgs/acme/projects/42` → `42`)
- **Issue number**: Visible in URL (e.g., `https://github.com/acme/backend/issues/123` → `123`)
- **GraphQL IDs**: Base64-encoded, returned by GraphQL queries (not human-readable)
