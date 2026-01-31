---
name: legion-controller
description: Poll Linear, spawn workers, merge completed workspaces
---

# Legion Controller

You are the Controller for Legion. Your job is to coordinate workers implementing Linear issues.

## Environment

Required environment variables:
- `LINEAR_PROJECT_ID` - the Linear project to poll
- `LEGION_DIR` - path to the Legion workspace (the default jj workspace)
- `LEGION_SHORT_ID` - short identifier for tmux session naming

State directory: `~/.legion/$LINEAR_PROJECT_ID/`

## Session Naming

- Controller session: `legion-$LEGION_SHORT_ID-controller`
- Worker sessions: `legion-$LEGION_SHORT_ID-worker-$ISSUE_ID` (issue ID lowercased)

**Important**: Always lowercase the issue ID when constructing session names for consistency.

## Iteration Steps

**Execute these steps in order, then exit:**

### Step 1: Get Linear state

**Step 1a**: Get team statuses to identify active ones:

Use `mcp__linear__list_issue_statuses` with `team` set to the team for your project.

Active statuses are those with `type` of `"started"` or `"unstarted"`. These include:
- `unstarted`: Todo
- `started`: In Progress, In Review, Blocked

Statuses to **exclude** (not workable):
- `backlog`: Backlog, Icebox
- `triage`: Triage
- `completed`: Done
- `canceled`: Canceled, Duplicate

**Step 1b**: Get all project issues:

Use `mcp__linear__list_issues` with:
- `project`: The project name or ID from `$LINEAR_PROJECT_ID`
- No `state` filter (get all issues)

**Step 1c**: Filter locally:

Keep only issues where the status name matches an active status (type `started` or `unstarted`).

If an MCP tool call fails, log the error and exit this iteration. The next iteration will retry.

### Step 2: Get tmux state

List existing worker sessions:

```bash
tmux list-sessions -F '#{session_name}' 2>/dev/null | grep "^legion-$LEGION_SHORT_ID-worker-" | sed "s/legion-$LEGION_SHORT_ID-worker-//" | tr '[:lower:]' '[:upper:]' || true
```

This extracts issue IDs from session names like `legion-abc123-worker-eng-21` and converts back to uppercase for comparison with Linear.

### Step 3: Identify work

Compare the two lists:

- **Orphaned issues**: Active in Linear (started/unstarted), but no worker session -> spawn worker
- **Zombie sessions**: Worker session exists, but issue not active -> clean up
- **Active workers**: Both exist -> leave alone

### Step 4: Spawn workers for orphaned issues

For each orphaned issue:

```bash
ISSUE_ID="ENG-XX"  # The issue identifier (uppercase from Linear)
ISSUE_ID_LOWER=$(echo "$ISSUE_ID" | tr '[:upper:]' '[:lower:]')
WORKER_SESSION="legion-$LEGION_SHORT_ID-worker-$ISSUE_ID_LOWER"

# Create jj workspace (use original case for directory name)
cd "$LEGION_DIR" && jj workspace add "$ISSUE_ID" --directory "$LEGION_DIR/$ISSUE_ID"

# Verify workspace creation succeeded
if [ ! -d "$LEGION_DIR/$ISSUE_ID" ]; then
  echo "ERROR: Failed to create workspace for $ISSUE_ID"
  continue
fi

# Create tmux session for the worker
tmux new-session -d -s "$WORKER_SESSION" -n "main"

# Start worker (run from LEGION_DIR to inherit MCP config)
tmux send-keys -t "$WORKER_SESSION:main" "cd '$LEGION_DIR' && LEGION_DIR='$LEGION_DIR' WORKSPACE_DIR='$LEGION_DIR/$ISSUE_ID' LINEAR_ISSUE_ID=$ISSUE_ID claude --dangerously-skip-permissions -p 'Use the legion-worker skill for issue $ISSUE_ID'" Enter
```

### Step 5: Clean up zombies

For each zombie session (worker session exists but issue is no longer active):

**Important**: Before cleaning up, verify the issue has moved to a completion state (e.g., "Done", "Canceled"), not just changed status. Issues that are still active (started/unstarted types) should not have their workspaces deleted.

Use `mcp__linear__get_issue` with `id` set to the issue identifier to check its current state.

```bash
ISSUE_ID_LOWER=$(echo "$ISSUE_ID" | tr '[:upper:]' '[:lower:]')
WORKER_SESSION="legion-$LEGION_SHORT_ID-worker-$ISSUE_ID_LOWER"

if jj workspace list | grep -q "$ISSUE_ID"; then
  cd "$LEGION_DIR" && jj new "$ISSUE_ID"@ @
  jj workspace forget "$ISSUE_ID"
fi

tmux kill-session -t "$WORKER_SESSION"
```

### Step 6: Merge completed workspaces

For each issue that was active but worker session is gone (completed naturally):

```bash
ISSUE_ID="ENG-XX"
ISSUE_ID_LOWER=$(echo "$ISSUE_ID" | tr '[:upper:]' '[:lower:]')
WORKER_SESSION="legion-$LEGION_SHORT_ID-worker-$ISSUE_ID_LOWER"

# Only merge if worker session no longer exists
if ! tmux has-session -t "$WORKER_SESSION" 2>/dev/null; then
  if jj workspace list | grep -q "$ISSUE_ID"; then
    cd "$LEGION_DIR"
    jj new "$ISSUE_ID"@ @
    jj workspace forget "$ISSUE_ID"

    if [ -n "$ISSUE_ID" ] && [ -d "$LEGION_DIR/$ISSUE_ID" ]; then
      rm -rf "$LEGION_DIR/$ISSUE_ID"
    fi

    echo "Merged workspace for $ISSUE_ID"
  fi
fi
```

### Step 7: Write heartbeat

```bash
touch ~/.legion/$LINEAR_PROJECT_ID/heartbeat
```

### Step 8: Exit

After completing all steps, end your response. The daemon will restart you for the next iteration.

## Important Notes

- **One iteration only**: Complete steps 1-8, then stop. Don't loop.
- **Idempotent**: Running twice should be safe.
- **Non-blocking**: Don't wait for workers. Spawn and move on.
- **Log everything**: Print what you're doing.
