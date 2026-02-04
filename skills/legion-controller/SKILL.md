---
name: legion-controller
description: Use when coordinating Legion workers across Linear issues - invoked with LINEAR_TEAM_ID, LEGION_DIR, and LEGION_SHORT_ID environment variables set
---

# Legion Controller

Persistent coordinator that loops forever, dispatching and resuming workers based on Linear issue state.

## Environment

Required:
- `LINEAR_TEAM_ID` - Linear team UUID
- `LEGION_DIR` - path to default jj workspace
- `LEGION_SHORT_ID` - short ID for tmux sessions

## Core Principle

**Keep work moving forward.** Priority order:
1. Unblock in-progress work (relay user feedback)
2. Advance completed work (process worker-done)
3. Start new work (triage, pull from Icebox)

## Algorithm

```dot
digraph controller {
    rankdir=TB;
    start [label="Start Loop"];
    fetch [label="1. Fetch Issues"];
    feedback [label="2. Relay Feedback"];
    worker_done [label="3. Process worker-done"];
    triage [label="4. Route Triage"];
    icebox [label="5. Pull Icebox"];
    cleanup [label="6. Cleanup Done"];
    heartbeat [label="7. Heartbeat"];
    todo [label="8. Update To-Do"];
    sleep [label="9. Sleep 30s"];
    start -> fetch -> feedback -> worker_done -> triage -> icebox -> cleanup -> heartbeat -> todo -> sleep -> fetch;
}
```

**Do not exit.** Loop continuously.

### 1. Fetch Issues

```bash
LINEAR_JSON=$(mcp__linear__list_issues team="$LINEAR_TEAM_ID" limit=100)
ACTIVE_WORKERS=$(tmux list-windows -t "legion-$LEGION_SHORT_ID" -F '#{window_name}' 2>/dev/null | grep -v "^main$" | wc -l)
```

### 2. Relay User Feedback (Highest Priority)

When both `user-input-needed` AND `user-feedback-given` labels present:
1. Remove both labels
2. **Resume** (not spawn) worker session with prompt to check Linear comments

### 3. Process worker-done

Run state script:
```bash
echo "$LINEAR_JSON" | python -m legion.state --team-id "$LINEAR_TEAM_ID" --short-id "$LEGION_SHORT_ID" --tmux-session "legion-$LEGION_SHORT_ID"
```

State transitions (always remove `worker-done` after):

| Current Status | Action |
|----------------|--------|
| Backlog + worker-done | → Todo, dispatch planner |
| Todo + worker-done | → In Progress, dispatch implementer |
| In Progress + worker-done | → Needs Review, dispatch reviewer |
| Needs Review + worker-done (PR ready) | → Retro, resume implementer |
| Needs Review + worker-done (PR draft) | Keep status, resume implementer for changes |
| Needs Review + worker-done (no PR) | `investigate_no_pr` - see below |
| Retro + worker-done | Dispatch merger |
| `remove_worker_active_and_redispatch` | Remove worker-active label, then dispatch |

**Handling `investigate_no_pr`:** Worker marked done but no PR exists. Likely causes:
1. Worker crashed before creating PR
2. PR creation failed silently
3. Issue moved to wrong status manually
4. Linear attachment wasn't added

**Action:** Investigate, then consider moving back to In Progress and re-dispatching implementer. May also just wait and check again next iteration.

### 4. Route Triage

Controller routes Triage issues directly (no worker needed):

| Assessment | Route To |
|------------|----------|
| Urgent AND clear requirements | Todo (dispatch planner) |
| Clear but not urgent | Backlog |
| Vague OR large OR needs breakdown | Icebox |

### 5. Pull from Icebox

**If active workers < 10:**
1. Get oldest Icebox item (FIFO)
2. Move to Backlog
3. Dispatch architect

### 6. Cleanup Done

For Done issues without live workers:
```bash
WORKSPACES_DIR=$(dirname "$LEGION_DIR")
ISSUE_LOWER=$(echo "$ISSUE_IDENTIFIER" | tr '[:upper:]' '[:lower:]')
jj workspace forget "$ISSUE_LOWER" -R "$LEGION_DIR"
rm -rf "$WORKSPACES_DIR/$ISSUE_LOWER"
```

### 7. Write Heartbeat

```bash
mkdir -p ~/.legion/$LEGION_SHORT_ID && touch ~/.legion/$LEGION_SHORT_ID/heartbeat
```

### 8. Update To-Do List

Maintain in context:
```markdown
## Controller State
**Active workers:** [count] / 10 max
### Priority Queue
- [ENG-XX] description
### In Progress
- [ENG-YY] mode - worker running
### Blocked
- [ENG-ZZ] user-input-needed
```

### 9. Sleep and Loop

```bash
sleep 30
```

Then return to step 1.

## Dispatch vs Resume

**Dispatch** = new worker window:
```bash
# $ISSUE_ID = Linear UUID, $ISSUE_IDENTIFIER = e.g. "LEG-18"
# Workspaces are siblings to default workspace, named by identifier for easy navigation
WORKSPACES_DIR=$(dirname "$LEGION_DIR")
ISSUE_LOWER=$(echo "$ISSUE_IDENTIFIER" | tr '[:upper:]' '[:lower:]')
WORKSPACE_PATH="$WORKSPACES_DIR/$ISSUE_LOWER"

SESSION_ID=$(python3 -c "import uuid; print(uuid.uuid5(uuid.UUID('$LINEAR_TEAM_ID'), '$ISSUE_IDENTIFIER:$MODE'))")
[ ! -d "$WORKSPACE_PATH" ] && jj workspace add "$WORKSPACE_PATH" --name "$ISSUE_LOWER" -R "$LEGION_DIR"

# Window name: issue-mode (e.g., leg-18-architect)
WINDOW_NAME="$ISSUE_LOWER-$MODE"
tmux new-window -t "legion-$LEGION_SHORT_ID" -n "$WINDOW_NAME" -d

# Start interactive Claude with skill invocation
tmux send-keys -t "legion-$LEGION_SHORT_ID:$WINDOW_NAME" \
    "cd '$WORKSPACE_PATH' && LINEAR_ISSUE_ID='$ISSUE_IDENTIFIER' claude --dangerously-skip-permissions --session-id '$SESSION_ID' '/legion-worker $MODE mode for $ISSUE_IDENTIFIER'" Enter

# Add worker-active label
mcp__linear__update_issue id="$ISSUE_ID" labels=["worker-active", ...existing...]
```

**Resume** = continue existing window:
```bash
ISSUE_LOWER=$(echo "$ISSUE_IDENTIFIER" | tr '[:upper:]' '[:lower:]')
WINDOW_NAME="$ISSUE_LOWER-$MODE"
tmux send-keys -t "legion-$LEGION_SHORT_ID:$WINDOW_NAME" "$PROMPT" Enter
```

Use resume for: user feedback relay, PR changes requested, retro after review approval.

## Worker Inspection

Available when needed (debugging, intervention):

```bash
# List worker windows
tmux list-windows -t "legion-$LEGION_SHORT_ID" -F '#{window_name}' | grep -v "^main$"

# Capture pane output
tmux capture-pane -t "legion-$LEGION_SHORT_ID:$WINDOW_NAME" -p

# Read session file
cat ~/.claude/projects/*/SESSION_ID.jsonl | tail -20

# Send input (use sparingly)
tmux send-keys -t "legion-$LEGION_SHORT_ID:$WINDOW_NAME" "message" Enter
```

## Labels

| Label | Meaning |
|-------|---------|
| `worker-done` | Worker finished phase, controller acts |
| `worker-active` | Worker dispatched and running |
| `user-input-needed` | Blocked on human, controller skips |
| `user-feedback-given` | Human responded, controller resumes |

## Common Mistakes

| Mistake | Correction |
|---------|------------|
| Spawn new worker for user feedback | **Resume** existing session with `--resume` |
| Skip Icebox when capacity exists | Pull oldest Icebox item if workers < 10 |
| Plan Triage items directly | Route first (to Icebox/Backlog/Todo), then workers act |
| Exit after processing all issues | **Never exit** - loop forever with 30s sleep |
| Process issue with live worker | Skip it - worker is already handling |

## Status Flow

```
Triage ─┬─► Icebox ─► Backlog ─► Todo ─► In Progress ─► Needs Review ─► Retro ─► Done
        ├─► Backlog ──────────────┘                          │
        └─► Todo ─────────────────────────────────────────────┘
```
