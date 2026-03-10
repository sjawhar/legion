# First-Class Workspace Management

**Date:** 2026-03-12
**Status:** Approved

## Problem

Legion's workspace model has three compounding issues:

1. **Workspace pollution**: `jj workspace add ... -R user-repo` registers workspaces in the user's jj repo store. `jj workspace list` in a personal checkout shows all Legion workspaces, cluttering the personal environment.

2. **Single-repo assumption**: One `LEGION_DIR` per daemon, one repo per project. But a single GitHub Project can track issues across multiple repositories (agent-c, legion, etc.). Legion can't route issues to different repos.

3. **Non-standard state location**: `~/.legion/` is hardcoded. Not XDG-compliant.

## Solution

Legion manages its own repo clones (fully isolated from personal checkouts), auto-discovers repos from issue metadata, uses XDG-compliant directories, and renames the core unit from "team" to "legion."

## Directory Layout

```
~/.local/share/legion/                    # XDG_DATA_HOME/legion
  repos/
    github.com/<owner>/<repo>/            # Legion's own jj clones (shared across legions)
  workspaces/
    <project-id>/
      <issue-id>/                         # jj workspaces, per-legion

~/.local/state/legion/                    # XDG_STATE_HOME/legion
  legions.json                            # {project-id: {port, servePort, pid, startedAt}}
  legions/
    <project-id>/
      workers.json
      logs/
      heartbeat
```

### Rationale

- **Repos keyed by `github.com/owner/repo`**: Mirrors `go get`/`ghq` conventions, supports future non-GitHub hosts.
- **Repos shared across legions**: A repo is a git clone — safe to share since jj workspaces provide isolation. Avoids cloning the same large repo multiple times.
- **Workspaces keyed by project-id then issue-id**: A workspace belongs to a legion, and that legion's controller manages its lifecycle.
- **State vs data split**: `XDG_STATE_HOME` for ephemeral operational state (workers.json, logs). `XDG_DATA_HOME` for persistent artifacts (repo clones, in-progress workspaces with uncommitted work).

### XDG Defaults

| Variable | Default | Contents |
|----------|---------|----------|
| `XDG_DATA_HOME` | `~/.local/share` | Repo clones, workspaces |
| `XDG_STATE_HOME` | `~/.local/state` | legions.json, workers.json, logs |

## Repo Management & Auto-Cloning

Workspace management moves into the daemon. The daemon resolves issue ID → repo → clone → workspace.

### Flow

```
Controller: legion dispatch owner-repo-42 implement
  ↓
CLI POSTs to daemon: {issueId: "owner-repo-42", mode: "implement"}
  ↓
Daemon resolves repo from issue ID:
  "owner-repo-42" → github.com/owner/repo
  ↓
Daemon ensures clone exists:
  ~/.local/share/legion/repos/github.com/owner/repo/
  If missing: jj git clone https://github.com/owner/repo <path>
  If exists:  jj git fetch -R <path>
  ↓
Daemon creates jj workspace if needed:
  jj workspace add <workspace-path> --name owner-repo-42 -R <clone-path>
  ↓
Daemon creates session with workspace path, returns worker entry
```

### Key Changes

- **`POST /workers` no longer requires `workspace` param** — daemon computes it from issue ID + project ID.
- **CLI dispatch becomes thinner** — just passes issue ID and mode.
- **Repo resolution from issue ID** — `owner-repo-42` → `github.com/owner/repo`. This parsing already exists conceptually in the controller skill.
- **Clone-on-demand** — first issue from a new repo triggers a clone. Subsequent issues reuse the existing clone with a fetch.
- **`LEGION_DIR` removed** — replaced by the repo pool. No single "base repo" concept.

## Naming: "Legion" not "Team"

A running instance (daemon + controller + workers, focused on one project) is a **legion**. The "team" terminology came from Linear; "legion" is the natural unit.

| Old | New |
|-----|-----|
| `teams.json` | `legions.json` |
| `~/.legion/{teamId}/` | `~/.local/state/legion/legions/{project-id}/` |
| `LEGION_TEAM_ID` | Retained as env var (it's the project identifier), but conceptually it identifies the legion |

## Multi-Legion Support

Multiple independent daemon instances, each managing its own project. No inter-daemon communication. Only shared resource is the repo pool.

### `legions.json`

```json
{
  "sjawhar/42": {"port": 13370, "servePort": 13381, "pid": 12345, "startedAt": "2026-03-12T10:00:00Z"},
  "other-org/7": {"port": 13371, "servePort": 13382, "pid": 12346, "startedAt": "2026-03-12T11:00:00Z"}
}
```

Project ID is the legion ID — no alias layer.

### Port Allocation

- Auto-assigned from base (13370, 13371, ...) by scanning `legions.json` for in-use ports.
- User can override with `--port` or `LEGION_DAEMON_PORT`.
- Serve port auto-assigned similarly (base 13381).
- Stale PID detection: if a legion's PID is dead, reclaim the port.

### CLI Routing

- **Controller → daemon**: `LEGION_DAEMON_PORT` env var (already works, daemon sets it in controller environment).
- **User → daemon**: CLI reads `legions.json` to find port by project ID.

```bash
legion start sjawhar/42          # Auto-assigns port, writes legions.json
legion status sjawhar/42         # Reads legions.json → port 13370
legion stop sjawhar/42           # Reads legions.json → port 13370
```

## Controller Changes

Minimal. The controller already includes repo info in dispatch prompts.

### What changes

- **Cleanup step** (step 6): No longer uses `WORKSPACES_DIR=$(dirname "$LEGION_DIR")`. Instead calls a daemon cleanup endpoint or new CLI command, since the daemon knows where workspaces live.
- **Working directory**: Controller runs in a generic directory (e.g., the state dir), not a repo. It's a coordinator, not a coder.
- **Remove `LEGION_DIR` references** from the skill.

### What stays the same

- Dispatch/resume prompt format (already includes backend and repo).
- Controller loop algorithm.
- State machine routing.

## Worker Changes

**None.** Workers already:
- Receive issue ID, mode, backend via prompt.
- Operate in whatever workspace the daemon placed them in.
- Use jj commands relative to their working directory.
- Push via `jj git push`.

Workers don't know or care where on disk their workspace lives.

## Component Change Summary

| File | Change |
|------|--------|
| `config.ts` | XDG path resolution, auto port assignment, remove `LEGION_DIR` as required |
| `cli/index.ts` | Read/write `legions.json`, remove workspace creation (daemon handles it), rename team→legion |
| `server.ts` | `POST /workers` resolves repo + workspace internally, add cleanup endpoint |
| `index.ts` | Controller runs in generic working dir, register/deregister in `legions.json` |
| `serve-manager.ts` | New functions for repo cloning, workspace creation |
| Controller skill | Remove `LEGION_DIR` references, use daemon for cleanup |
| Worker skill | No changes |

## Migration

No migration. Clean break. Users `rm -rf ~/.legion` and start fresh. All work-in-progress is pushed to GitHub; repo clones are re-created on demand.

## What Stays the Same

- Worker dispatch/resume protocol
- Session ID computation (deterministic UUIDv5)
- Worker entry schema (`workspace` field points to new location for new workers)
- Controller loop algorithm
- All worker workflows (architect, plan, implement, test, review, merge)
