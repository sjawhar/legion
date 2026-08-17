---
name: linear
description: Manage Linear issues. Use when working with tasks, tickets, bugs, or Linear.
mcp:
  linear:
    command: npx
    args: ["-y", "github:obra/streamlinear"]
    env:
      LINEAR_API_TOKEN: ${LINEAR_API_TOKEN}
---

# Linear (Stream Linear)

Single-tool MCP with action dispatch. All operations go through `linear_linear`.

## Actions

### Search Issues

```
linear_linear(action="search")                          # Your active issues
linear_linear(action="search", query="auth bug")        # Text search
linear_linear(action="search", query={state: "In Progress"})  # Filter
linear_linear(action="search", query={team: "ENG", assignee: "me"})
```

### Get Issue Details

```
linear_linear(action="get", id="ABC-123")               # By short ID
linear_linear(action="get", id="https://linear.app/...")  # By URL
```

Returns: title, description, status, labels, comments, attachments.

### Update Issue

```
linear_linear(action="update", id="ABC-123", state="Done")
linear_linear(action="update", id="ABC-123", priority=1)
linear_linear(action="update", id="ABC-123", assignee="me")
linear_linear(action="update", id="ABC-123", labels=["worker-done", "existing-label"])
```

**Labels array replaces all labels.** Fetch current labels first, then append.

### Comment on Issue

```
linear_linear(action="comment", id="ABC-123", body="Fixed in commit abc123")
```

### Create Issue

```
linear_linear(action="create", title="Bug: Login fails", team="ENG")
linear_linear(action="create", title="Bug", team="ENG", body="Details", priority=2)
```

### Raw GraphQL

```
linear_linear(action="graphql", graphql="query { viewer { name } }")
```

### Help

```
linear_linear(action="help")
```

## Reference

- Priority: 0=None, 1=Urgent, 2=High, 3=Medium, 4=Low
- State matching is fuzzy: "done" → "Done", "in prog" → "In Progress"
- IDs accept: `ABC-123`, Linear URLs, or UUIDs
