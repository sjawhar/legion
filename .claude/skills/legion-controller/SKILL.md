---
name: legion-controller
description: Use when coordinating Legion workers across Linear issues - invoked with LINEAR_TEAM_ID, LEGION_DIR, LEGION_SHORT_ID, and LEGION_DAEMON_PORT environment variables set
---

# Legion Controller

Persistent coordinator that loops forever, dispatching and resuming workers based on Linear issue state.

## Environment

Required:
- `LINEAR_TEAM_ID` - Linear team UUID
- `LEGION_DIR` - path to default jj workspace
- `LEGION_SHORT_ID` - short ID for daemon identification
- `LEGION_DAEMON_PORT` - daemon HTTP API port (default: 13370)

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
LINEAR_JSON=$(linear_linear(action="search", query={"team": "$LINEAR_TEAM_ID"}))
ACTIVE_WORKERS=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers | jq 'length')
```

### 2. Relay User Feedback (Highest Priority)

When both `user-input-needed` AND `user-feedback-given` labels present:
1. Remove both labels
2. **Resume** (not spawn) worker session with prompt to check Linear comments

### 3. Process worker-done

Run state script:
```bash
echo "$LINEAR_JSON" | bun run packages/daemon/src/state/cli.ts --team-id "$LINEAR_TEAM_ID" --daemon-url http://127.0.0.1:$LEGION_DAEMON_PORT
```

State transitions (always remove `worker-done` after):

| Current Status | Action |
|----------------|--------|
| Backlog + worker-done | → Add `needs-approval`, remove `worker-done` (pause for human) |
| Backlog + needs-approval + human-approved | → Todo, remove both labels, dispatch planner |

> **Note:** The approval gate only applies to automated transitions. If a human manually moves an issue from Backlog → Todo in Linear, this is treated as an intentional bypass — the controller proceeds normally without requiring `human-approved`.
| Todo + worker-done | → In Progress, dispatch implementer |
| In Progress + worker-done | Quality gate check → if pass: → Needs Review, dispatch reviewer. If fail: resume implementer with fix instructions |
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

### Quality Gate Check

When the state machine returns `preCheck: "quality-gate"` (on In Progress + worker-done transitions), the controller must verify the worker's code before advancing.

**Trust but verify:** The implement workflow self-enforces checks before PR (step 4). The controller independently verifies.

```bash
WORKSPACES_DIR=$(dirname "$LEGION_DIR")
ISSUE_LOWER=$(echo "$ISSUE_IDENTIFIER" | tr '[:upper:]' '[:lower:]')
WORKSPACE_PATH="$WORKSPACES_DIR/$ISSUE_LOWER"

cd "$WORKSPACE_PATH"
bun test 2>&1
TST_EXIT=$?
bunx tsc --noEmit 2>&1
TSC_EXIT=$?
bunx biome check 2>&1
BIOME_EXIT=$?
```

**If all pass** (exit codes 0): Proceed with transition to Needs Review.

**If any fail:** Do NOT advance to Needs Review. Instead:
1. Remove `worker-done` label
2. Resume the implementer session with the failure output
3. The implementer will fix and re-add `worker-done` when ready

**Reading preCheck:** After calling the state CLI, parse the returned JSON. If `preCheck` is present, run the quality gate before executing the suggested action. Only proceed with the action if the gate passes.

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

**Dispatch** = new worker via daemon API:
```bash
# $ISSUE_ID = Linear UUID, $ISSUE_IDENTIFIER = e.g. "LEG-18"
# Workspaces are siblings to default workspace, named by identifier for easy navigation
WORKSPACES_DIR=$(dirname "$LEGION_DIR")
ISSUE_LOWER=$(echo "$ISSUE_IDENTIFIER" | tr '[:upper:]' '[:lower:]')
WORKSPACE_PATH="$WORKSPACES_DIR/$ISSUE_LOWER"

# Create workspace if needed
[ ! -d "$WORKSPACE_PATH" ] && jj workspace add "$WORKSPACE_PATH" --name "$ISSUE_LOWER" -R "$LEGION_DIR"

# Dispatch worker via daemon HTTP API
RESPONSE=$(curl -s -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/workers \
  -H 'content-type: application/json' \
  -d "{
    \"issueId\": \"$ISSUE_IDENTIFIER\",
    \"mode\": \"$MODE\",
    \"workspace\": \"$WORKSPACE_PATH\",
    \"env\": {
      \"LINEAR_ISSUE_ID\": \"$ISSUE_IDENTIFIER\",
      \"LINEAR_TEAM_ID\": \"$LINEAR_TEAM_ID\",
      \"LEGION_DIR\": \"$LEGION_DIR\",
      \"LEGION_SHORT_ID\": \"$LEGION_SHORT_ID\"
    }
  }")

WORKER_ID=$(echo "$RESPONSE" | jq -r '.id')
WORKER_PORT=$(echo "$RESPONSE" | jq -r '.port')
SESSION_ID=$(echo "$RESPONSE" | jq -r '.sessionId')

# Send initial prompt to worker's OpenCode serve
curl -s -X POST http://127.0.0.1:$WORKER_PORT/session/$SESSION_ID/prompt_async \
  -H 'content-type: application/json' \
  -d "{
    \"parts\": [{
      \"type\": \"text\",
      \"text\": \"/legion-worker $MODE mode for $ISSUE_IDENTIFIER\"
    }]
  }"

# Add worker-active label
linear_linear(action="update", id="$ISSUE_ID", labels=["worker-active", ...existing...])
```

**Resume** = send prompt to existing worker:
```bash
# Get worker info from daemon
ISSUE_LOWER=$(echo "$ISSUE_IDENTIFIER" | tr '[:upper:]' '[:lower:]')
WORKER_ID="$ISSUE_LOWER-$MODE"
WORKER_INFO=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID)
WORKER_PORT=$(echo "$WORKER_INFO" | jq -r '.port')
SESSION_ID=$(echo "$WORKER_INFO" | jq -r '.sessionId')

# Send prompt to worker's OpenCode serve
curl -s -X POST http://127.0.0.1:$WORKER_PORT/session/$SESSION_ID/prompt_async \
  -H 'content-type: application/json' \
  -d "{
    \"parts\": [{
      \"type\": \"text\",
      \"text\": \"$PROMPT\"
    }]
  }"
```

Use resume for: user feedback relay, PR changes requested, retro after review approval.

## Worker Inspection

Available when needed (debugging, intervention):

```bash
# List all workers
curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers | jq '.[] | {id, status, port, pid, sessionId, startedAt}'

# Get specific worker info
curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID | jq '.'

# Check worker status (busy/idle)
curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID/status | jq '.'

# Get worker messages
WORKER_PORT=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID | jq -r '.port')
curl -s http://127.0.0.1:$WORKER_PORT/session/$SESSION_ID/message?limit=20 | jq '.[-5:]'

# Send input (use sparingly - prefer prompt_async)
curl -s -X POST http://127.0.0.1:$WORKER_PORT/session/$SESSION_ID/prompt_async \
  -H 'content-type: application/json' \
  -d '{"parts":[{"type":"text","text":"message"}]}'
```

## Labels

| Label | Meaning |
|-------|---------|
| `worker-done` | Worker finished phase, controller acts |
| `worker-active` | Worker dispatched and running |
| `user-input-needed` | Blocked on human, controller skips |
| `user-feedback-given` | Human responded, controller resumes |
| `needs-approval` | Architect done, waiting for human approval |
| `human-approved` | Human approved, controller advances to planner |

## Common Mistakes

| Mistake | Correction |
|---------|------------|
| Spawn new worker for user feedback | **Resume** existing session via HTTP API |
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
