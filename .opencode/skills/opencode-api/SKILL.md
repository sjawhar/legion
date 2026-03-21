---
name: opencode-api
description: OpenCode serve HTTP API reference. Use when interacting with sessions, reading messages/todos, aborting workers, managing workspaces, or any direct OpenCode serve API call. The shared serve runs on port 13381 and handles all worker and controller sessions.
---

# OpenCode Serve API

HTTP API for the shared OpenCode serve process. All worker and controller sessions run on a
single `opencode serve` instance (default port **13381**).

**When to use this skill:**
- Reading session messages or todos (diagnostics)
- Aborting a stuck session
- Checking session status directly
- Creating/managing sessions outside the daemon
- Workspace or worktree operations
- Any endpoint the daemon API doesn't expose

**When to use the daemon API instead:**
- `POST /workers` — dispatch a new worker (daemon handles session creation + workspace)
- `GET /workers` — list workers
- `POST /state/collect` — state machine decisions
- `GET /health` — daemon health

## Architecture

```
Controller ──► Daemon API (port $LEGION_DAEMON_PORT, default 13370)
                  │
                  ├── POST /workers        → creates session on shared serve
                  ├── POST /workers/:id/prompt → proxies to session.promptAsync()
                  ├── GET /workers/:id/status  → proxies to session.status()
                  └── POST /state/collect  → state machine
                  │
                  ▼
              Shared Serve (port 13381)
                  │
                  ├── Session 1 (controller)
                  ├── Session 2 (worker: eng-42-implement)
                  ├── Session 3 (worker: eng-42-review)
                  └── ...
```

## Port Discovery

The shared serve port is **13381** by default. To discover it programmatically:

```bash
# From any worker entry returned by the daemon
SERVE_PORT=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers | jq '.[0].port')
```

## Request Scoping

Every request must be scoped to a project directory. Use **either**:

```bash
# Option 1: Query parameter (recommended for curl)
curl -s "http://127.0.0.1:13381/session?directory=/path/to/workspace"

# Option 2: Header
curl -s http://127.0.0.1:13381/session \
  -H "x-opencode-directory: /path/to/workspace"
```

The workspace path comes from the worker entry:
```bash
WORKSPACE=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID | jq -r '.workspace')
```

## Key Patterns

### Create a Session

```bash
curl -s -X POST "http://127.0.0.1:13381/session?directory=$WORKSPACE" \
  -H 'Content-Type: application/json' \
  -d '{"id": "ses_deterministic_id"}'
```

- Returns `{"id": "ses_...", "title": "", ...}` on success
- Returns **409** `DuplicateIDError` if session exists (idempotent — reuse it)
- Session IDs are deterministic UUIDs from `computeSessionId(legionId, issueId, mode)`

### Send a Prompt (Async)

```bash
curl -s -X POST "http://127.0.0.1:13381/session/$SESSION_ID/prompt_async?directory=$WORKSPACE" \
  -H 'Content-Type: application/json' \
  -d '{"parts": [{"type": "text", "text": "Your prompt here"}]}'
```

- Returns **204** immediately (fire-and-forget)
- The session processes the prompt when its current turn completes
- Use for: resuming workers, relaying feedback, triggering retro

### Check Session Status

```bash
# All sessions
curl -s "http://127.0.0.1:13381/session/status?directory=$WORKSPACE"

# Returns map: { "ses_abc": {"type": "busy"}, "ses_def": {"type": "idle"} }
```

Status types:
- `{"type": "idle"}` — session is waiting for input
- `{"type": "busy"}` — session is processing
- `{"type": "retry", "attempt": N, "message": "...", "next": timestamp}` — retrying after error

### Read Session Messages

```bash
# All messages
curl -s "http://127.0.0.1:13381/session/$SESSION_ID/message?directory=$WORKSPACE"

# Last N messages
curl -s "http://127.0.0.1:13381/session/$SESSION_ID/message?directory=$WORKSPACE&limit=5"

# Message count
curl -s "http://127.0.0.1:13381/session/$SESSION_ID/message/count?directory=$WORKSPACE"
```

Response is an array of `{info, parts}` objects. Each message has:
- `info.role` — `"user"` or `"assistant"`
- `info.time.created` — timestamp
- `parts[]` — array of text, tool calls, files, etc.

### Read Session Todos

```bash
curl -s "http://127.0.0.1:13381/session/$SESSION_ID/todo?directory=$WORKSPACE"
```

Returns array of `{content, status, priority}` objects. Status: `pending`, `in_progress`, `completed`, `cancelled`.

### Abort a Session

```bash
curl -s -X POST "http://127.0.0.1:13381/session/$SESSION_ID/abort?directory=$WORKSPACE"
```

Stops any ongoing AI processing. Returns `true` on success.

### Get Session Details

```bash
curl -s "http://127.0.0.1:13381/session/$SESSION_ID?directory=$WORKSPACE"
```

Returns full session object including title, timestamps, workspace, summary (file changes).

### List All Sessions

```bash
curl -s "http://127.0.0.1:13381/session?directory=$WORKSPACE"

# Filter: only root sessions (no children)
curl -s "http://127.0.0.1:13381/session?directory=$WORKSPACE&roots=true"

# Filter: sessions updated after timestamp
curl -s "http://127.0.0.1:13381/session?directory=$WORKSPACE&start=1710000000000"
```

### Get File Changes (Diff)

```bash
curl -s "http://127.0.0.1:13381/session/$SESSION_ID/diff?directory=$WORKSPACE"
```

Returns array of `{file, before, after, additions, deletions, status}`.

### Compact/Summarize a Session

```bash
curl -s -X POST "http://127.0.0.1:13381/session/$SESSION_ID/summarize?directory=$WORKSPACE" \
  -H 'Content-Type: application/json' \
  -d '{"providerID": "anthropic", "modelID": "claude-sonnet-4-20250514", "auto": true}'
```

Use when a session is running out of context window.

### Health Check

```bash
curl -s http://127.0.0.1:13381/global/health
# {"healthy": true, "version": "..."}
```

### Graceful Shutdown

```bash
curl -s -X POST http://127.0.0.1:13381/global/dispose
```

Disposes all sessions and cleans up resources. Best-effort (3s timeout).

## Workspace & Worktree Management

### Create a Worktree

```bash
curl -s -X POST "http://127.0.0.1:13381/experimental/worktree?directory=$WORKSPACE" \
  -H 'Content-Type: application/json' \
  -d '{"name": "feature-branch"}'
```

### List Worktrees

```bash
curl -s "http://127.0.0.1:13381/experimental/worktree?directory=$WORKSPACE"
```

### Remove a Worktree

```bash
curl -s -X DELETE "http://127.0.0.1:13381/experimental/worktree?directory=$WORKSPACE" \
  -H 'Content-Type: application/json' \
  -d '{"directory": "/path/to/worktree"}'
```

## Events (SSE)

Subscribe to real-time events:

```bash
curl -s -N "http://127.0.0.1:13381/event?directory=$WORKSPACE"
```

Key event types:
- `session.status` — session busy/idle transitions
- `session.idle` — session finished processing
- `message.updated` — new message created
- `message.part.updated` — tool call completed, text streamed
- `todo.updated` — todo list changed
- `permission.asked` — permission request pending
- `question.asked` — question pending

## Error Handling

| Status | Meaning |
|--------|---------|
| 200 | Success |
| 204 | Accepted (prompt_async) |
| 400 | Bad request (invalid JSON, missing fields) |
| 404 | Session/resource not found |
| 409 | Duplicate ID (session already exists — safe to reuse) |

## For Complete Endpoint Reference

See [reference.md](reference.md) for all endpoints with full request/response schemas.
