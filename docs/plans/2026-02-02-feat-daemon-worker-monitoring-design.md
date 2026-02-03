---
title: Daemon Worker Monitoring
type: feat
date: 2026-02-02
---

# Daemon Worker Monitoring Design

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Enable recovery from hung workers by having the daemon monitor worker windows and kill stale ones, while the controller re-dispatches naturally.

**Architecture:** Daemon manages local state (workspaces, windows, session files); Controller manages Linear state (issues, labels, dispatch). Workers run as windows within the controller session, not separate sessions.

**Tech Stack:** Python (anyio), tmux, Linear MCP

---

## Overview

### Problem

Currently the daemon only monitors the controller. If a worker hangs (Claude stuck, session file stopped updating, tmux window still alive), nobody notices and the issue remains stuck.

### Solution

1. **Workers as windows**: Workers run as windows within `legion-{short}-controller` session, not separate sessions
2. **Daemon monitors workers**: Enumerate windows, check session file mtimes, kill stale windows
3. **Controller re-dispatches**: State script detects orphaned issues, controller dispatches fresh workers

### Separation of Concerns

| Component | Responsibilities |
|-----------|-----------------|
| **Daemon** | Local state: workspaces, windows, session files. Kills stale windows. |
| **Controller** | Linear state: issues, labels. Dispatches workers. Uses Python state script. |
| **State Script** | Queries Linear + local state, suggests actions. |

---

## Architecture

### Window Naming Convention

Workers use window names: `{mode}-{issue}`

Examples:
- `implement-eng-123`
- `architect-leg-5`
- `review-eng-456`
- `plan-leg-12`

The controller session always has a `main` window for the controller itself.

### Session ID Computation

Session IDs are always derivable from `(team_id, issue_id, mode)`:

```python
import uuid

def compute_session_id(team_id: str, issue_id: str, mode: str) -> str:
    namespace = uuid.UUID(team_id)
    return str(uuid.uuid5(namespace, f"{issue_id}:{mode}"))
```

**Never store session IDs** - always recompute from window name.

### Label Lifecycle

| Label | Added | Removed | Purpose |
|-------|-------|---------|---------|
| `worker-active` | Controller on dispatch | Worker on completion | Tracks active work |
| `worker-done` | Worker on phase completion | Controller after transition | Signals phase complete |
| `user-input-needed` | Worker when blocked | Controller on feedback relay | Blocks re-dispatch |
| `user-feedback-given` | User after responding | Controller on feedback relay | Triggers resume |

### Data Sources

| Source | What It Tells Us |
|--------|-----------------|
| Linear issues | Status, labels (controller only) |
| tmux windows | Which workers are "alive" |
| Workspaces on disk | Which issues have been started |
| Session files | Activity level (mtime), conversation history |

---

## Daemon Worker Monitoring

### Algorithm

```python
async def check_worker_health(
    tmux_session: str,
    team_id: str,
    staleness_threshold: int = 600,
) -> None:
    """Check worker health and kill stale windows."""
    windows = await tmux.list_windows(tmux_session)

    for window in windows:
        if window == "main":
            continue  # Skip controller window

        # Parse {mode}-{issue} from window name
        parts = window.split("-", 1)
        if len(parts) != 2:
            continue  # Invalid format, skip

        mode, issue_id = parts
        issue_id = issue_id.upper()  # Normalize

        # Compute session file path
        session_id = compute_session_id(team_id, issue_id, mode)
        workspace = Path(workspace_dir) / issue_id
        session_file = get_session_file_path(workspace, session_id)

        # Check staleness
        mtime = await get_newest_mtime(session_file)
        if mtime is None:
            # No session file yet - worker just started
            continue

        age = time.time() - mtime
        if age > staleness_threshold:
            print(f"Killing stale worker: {window} (age: {age:.0f}s)")
            await tmux.kill_window(tmux_session, window)
```

### Integration with Health Loop

```python
async def health_loop(...) -> None:
    while True:
        await anyio.sleep(check_interval)

        # Check controller health (existing)
        if await controller_needs_restart(...):
            # ... restart controller ...

        # Check worker health (new)
        await check_worker_health(
            tmux_session=session,
            team_id=project_id,
            staleness_threshold=staleness_threshold,
        )
```

### What Daemon Does NOT Do

- Query Linear (controller's job)
- Manage labels (controller's job)
- Delete session files (never delete these)
- Delete workspaces (controller handles cleanup)
- Re-dispatch workers (controller's job)

---

## State Script Updates

### Orphan Detection

The state script already handles the case where `has_live_worker=False`. Add `worker-active` label detection:

```python
def build_issue_state(data: FetchedIssueData, team_id: str) -> IssueState:
    # Existing logic handles user-input-needed / user-feedback-given

    # Detect orphaned workers: worker-active but no live window
    has_worker_active = "worker-active" in data.labels

    if has_worker_active and not data.has_live_worker:
        # Worker died - remove label and re-dispatch
        action = "remove_worker_active_and_redispatch"
    else:
        action = suggest_action(...)
```

### Live Worker Detection

Update `get_live_workers()` to check windows instead of sessions:

```python
async def get_live_workers(short_id: str, tmux_session: str) -> dict[str, str]:
    """Get live workers as {issue_id: mode} from tmux windows."""
    windows = await tmux.list_windows(tmux_session)
    workers: dict[str, str] = {}

    for window in windows:
        if window == "main":
            continue

        parts = window.split("-", 1)
        if len(parts) == 2:
            mode, issue_id = parts
            workers[issue_id.upper()] = mode

    return workers
```

---

## Controller SKILL.md Updates

### Dispatch (Updated)

```bash
# Compute session ID
SESSION_ID=$(python3 -c "import uuid; print(uuid.uuid5(uuid.UUID('$LINEAR_TEAM_ID'), '$ISSUE_ID:$MODE'))")

# Create workspace if needed
[ ! -d "$LEGION_DIR/$ISSUE_ID" ] && jj workspace add "$LEGION_DIR/$ISSUE_ID" --name "$ISSUE_ID" -R "$LEGION_DIR"

# Create worker as WINDOW in controller session (not new session)
tmux new-window -t "legion-$LEGION_SHORT_ID-controller" -n "$MODE-$(echo $ISSUE_ID | tr '[:upper:]' '[:lower:]')" -d

# Send command to new window
tmux send-keys -t "legion-$LEGION_SHORT_ID-controller:$MODE-$(echo $ISSUE_ID | tr '[:upper:]' '[:lower:]')" \
    "cd '$LEGION_DIR/$ISSUE_ID' && LINEAR_ISSUE_ID='$ISSUE_ID' claude --dangerously-skip-permissions --session-id '$SESSION_ID' -p 'Use legion-worker skill in $MODE mode for $ISSUE_ID'" Enter

# Add worker-active label
mcp__linear__update_issue id="$ISSUE_ID" labels=["worker-active", ...existing...]
```

### Resume (Updated)

```bash
# Resume in existing window
tmux send-keys -t "legion-$LEGION_SHORT_ID-controller:$MODE-$(echo $ISSUE_ID | tr '[:upper:]' '[:lower:]')" \
    "cd '$LEGION_DIR/$ISSUE_ID' && LINEAR_ISSUE_ID='$ISSUE_ID' claude --dangerously-skip-permissions --resume '$SESSION_ID' -p '$PROMPT'" Enter
```

### Worker Completion

Workers must remove `worker-active` label when done:

```bash
# In legion-worker SKILL.md, on phase completion:
mcp__linear__update_issue id="$LINEAR_ISSUE_ID" labels=["worker-done"]  # Removes worker-active
```

---

## State Matrix Analysis

### 2x2x2: (workspace, window, session_file)

| Workspace | Window | Session File | Meaning | Action |
|-----------|--------|--------------|---------|--------|
| No | No | No | Never started | Dispatch |
| No | No | Yes | Impossible (session requires workspace) | - |
| No | Yes | No | Bug: window without workspace | Kill window |
| No | Yes | Yes | Impossible | - |
| Yes | No | No | Started but worker died before writing | Clean up or dispatch |
| Yes | No | Yes | Worker exited normally or was killed | Check Linear status |
| Yes | Yes | No | Worker just started | Wait |
| Yes | Yes | Yes (fresh) | Worker running normally | Skip |
| Yes | Yes | Yes (stale) | Worker hung | Kill window |

### Recovery Flows

**Hung worker:**
1. Daemon detects stale session file mtime
2. Daemon kills window
3. State script sees `worker-active` + no live window
4. Controller removes `worker-active`, re-dispatches

**Worker crashed (immediate):**
1. Window disappears
2. State script sees `worker-active` + no live window
3. Controller removes `worker-active`, re-dispatches

**Worker waiting for user input:**
1. Worker adds `user-input-needed`, exits
2. Window closes (worker exited)
3. State script sees `user-input-needed` → skips re-dispatch
4. User responds, adds `user-feedback-given`
5. Controller resumes worker

---

## Implementation Tasks

### Task 1: Update tmux.py for window operations

**Files:**
- Modify: `src/legion/tmux.py`

- [ ] Ensure `list_windows()` works correctly
- [ ] Ensure `kill_window()` works correctly
- [ ] Add `new_window()` function if needed

### Task 2: Add worker health check to daemon

**Files:**
- Modify: `src/legion/daemon.py`

- [ ] Add `check_worker_health()` function
- [ ] Parse window names `{mode}-{issue}`
- [ ] Compute session IDs from window names
- [ ] Check session file mtimes
- [ ] Kill stale windows
- [ ] Integrate into `health_loop()`

### Task 3: Update state types

**Files:**
- Modify: `src/legion/state/types.py`

- [ ] Add `worker-active` label constant
- [ ] Update `FetchedIssueData` if needed

### Task 4: Update state fetch for window-based workers

**Files:**
- Modify: `src/legion/state/fetch.py`

- [ ] Change `get_live_workers()` to check windows, not sessions
- [ ] Return `dict[str, str]` mapping issue_id → mode

### Task 5: Update state decision for orphan detection

**Files:**
- Modify: `src/legion/state/decision.py`

- [ ] Add `remove_worker_active_and_redispatch` action
- [ ] Handle `worker-active` without live window case

### Task 6: Update controller SKILL.md

**Files:**
- Modify: `skills/legion-controller/SKILL.md`

- [ ] Update dispatch to create windows, not sessions
- [ ] Add `worker-active` label on dispatch
- [ ] Update window naming: `{mode}-{issue}`
- [ ] Update resume to use windows

### Task 7: Update worker SKILL.md

**Files:**
- Modify: `skills/legion-worker/SKILL.md`

- [ ] Remove `worker-active` label on phase completion
- [ ] Document exit behavior for `user-input-needed`

### Task 8: Write tests

**Files:**
- Create/Modify: `tests/test_daemon.py`
- Create/Modify: `tests/test_state.py`

- [ ] Test `check_worker_health()` with mock tmux
- [ ] Test orphan detection in state machine
- [ ] Test window name parsing

---

## References

- `src/legion/daemon.py:56-76` - Existing `get_newest_mtime()` implementation
- `src/legion/daemon.py:131-154` - Existing `controller_needs_restart()` pattern
- `src/legion/state/decision.py:22-94` - State machine logic
- `src/legion/state/types.py:24` - `compute_session_id()` implementation
