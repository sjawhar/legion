# OpenCode Serve API — Complete Endpoint Reference

Base URL: `http://127.0.0.1:$SERVE_PORT`. Each `opencode serve` process listens on its own port — there is no single global serve. Examples below use **13381** as a placeholder (the default for Legion's shared serve, which may differ if multiple daemons coexist). Substitute the actual port for the serve you're targeting; for Legion, discover via the daemon's `/workers` endpoint.

All per-project endpoints accept `?directory=/path` query param or `x-opencode-directory` header.

---

## Global

### GET /global/health

Health check. No directory scoping needed.

```
Response 200:
{
  "healthy": true,
  "version": "0.0.3"
}
```

### GET /global/event

SSE stream of global events (all projects). Each event has `{directory, payload}`.

### GET /global/config

Get global configuration.

### PATCH /global/config

Update global configuration. Body: partial `Config` object.

### POST /global/dispose

Dispose all instances. Releases all resources. Returns `true`.

### GET /doc

OpenAPI 3.1 spec for the running serve, as JSON. Authoritative source of truth for which endpoints this serve actually exposes — it reflects the running build, including sami-fork additions and experimental routes, so consult it when this reference document might be stale.

```bash
# List every path the serve exposes
curl -s http://127.0.0.1:13381/doc | jq -r '.paths | keys[]'

# Inspect a specific operation's request schema
curl -s http://127.0.0.1:13381/doc \
  | jq '.paths["/session/{sessionID}/revert"].post'
```

No auth required when the serve runs without `ServerAuth.required`. The HTML UI lives at `/` and `/api/openapi` — the JSON spec is only at `/doc`.

---

## Sessions

### GET /session

List sessions. Sorted by most recently updated.

| Param | Type | Description |
|-------|------|-------------|
| `roots` | boolean | Only root sessions (no parentID) |
| `start` | number | Sessions updated on/after this timestamp (ms epoch) |
| `search` | string | Filter by title (case-insensitive) |
| `limit` | number | Max sessions to return |

```
Response 200: Session[]
```

### POST /session

Create a new session.

```json
Body:
{
  "id": "ses_optional_custom_id",
  "parentID": "ses_parent_if_fork",
  "title": "Optional title",
  "workspaceID": "wrk_optional"
}
```

All fields optional. `id` must match `^ses.*` pattern.

```
Response 200: Session
Response 409: DuplicateIDError (safe to reuse existing session)
```

### GET /session/status

Get status of ALL sessions.

```
Response 200:
{
  "ses_abc123": {"type": "idle"},
  "ses_def456": {"type": "busy"},
  "ses_ghi789": {"type": "retry", "attempt": 2, "message": "rate limited", "next": 1710000000}
}
```

### GET /session/{sessionID}

Get single session details.

```
Response 200: Session
Response 404: NotFoundError
```

Session object:
```json
{
  "id": "ses_...",
  "slug": "...",
  "projectID": "...",
  "directory": "/path/to/workspace",
  "title": "Session title",
  "version": "...",
  "time": {
    "created": 1710000000,
    "updated": 1710000100
  },
  "summary": {
    "additions": 42,
    "deletions": 10,
    "files": 3,
    "diffs": [...]
  }
}
```

### PATCH /session/{sessionID}

Update session properties.

```json
Body:
{
  "title": "New title",
  "time": {"archived": 1710000000}
}
```

### DELETE /session/{sessionID}

Delete session and all messages permanently.

### POST /session/{sessionID}/abort

Abort active processing. Returns `true`.

### POST /session/{sessionID}/fork

Fork session at a specific message.

```json
Body:
{
  "messageID": "msg_optional_fork_point"
}
```

Returns new `Session`.

### POST /session/{sessionID}/revert

Soft-revert the session to a specific message (or a specific part within a message). Sets `session.revert = {messageID, partID?, snapshot, diff}`, takes a worktree snapshot if patches were applied, and hides everything after the revert point from prompt context. Messages/parts are NOT deleted — `unrevert` restores them. Subsequent prompts trigger a `cleanup` step that permanently removes the hidden messages/parts via `message.removed` / `message.part.removed` events.

```json
Body:
{
  "messageID": "msg_target",
  "partID": "prt_target_optional"
}
```

- `messageID` required — the message to revert to (inclusive).
- `partID` optional — revert to a specific part within that message. If omitted, the entire message is reverted; the actual revert anchor walks back to the last user message before `messageID`. **The TUI revert hotkey only sets `messageID`** — pass `partID` here for finer-grained revert (e.g. roll back one tool call without losing the surrounding text and reasoning parts).
- Returns the updated `Session` (with `revert` field populated).

### POST /session/{sessionID}/unrevert

Clear the revert state and restore any worktree snapshot it captured. No-op if `session.revert` is already null. Returns the updated `Session`.

### POST /session/{sessionID}/summarize

Compact session to preserve key information.

```json
Body:
{
  "providerID": "anthropic",
  "modelID": "claude-sonnet-4-20250514",
  "auto": true
}
```

### GET /session/{sessionID}/children

Get child sessions forked from this session.

### GET /session/{sessionID}/diff

Get file changes for the session (or specific message).

| Param | Type | Description |
|-------|------|-------------|
| `messageID` | string | Filter to specific message's changes |

```
Response 200: FileDiff[]
{
  "file": "src/index.ts",
  "before": "...",
  "after": "...",
  "additions": 5,
  "deletions": 2,
  "status": "modified"  // added | deleted | modified
}
```

---

## Messages

### GET /session/{sessionID}/message

Get session messages.

| Param | Type | Description |
|-------|------|-------------|
| `limit` | integer | Max messages to return |
| `before` | string | Cursor for pagination |

```
Response 200: Array of {info: Message, parts: Part[]}
```

Message types:
- **UserMessage**: `{role: "user", agent, model, ...}`
- **AssistantMessage**: `{role: "assistant", modelID, providerID, cost, tokens, ...}`

Part types:
- **TextPart**: `{type: "text", text: "..."}`
- **ToolPart**: `{type: "tool", tool: "bash", state: {status: "completed", input: {...}, output: "..."}}`
- **FilePart**: `{type: "file", mime: "...", url: "..."}`
- **SubtaskPart**: `{type: "subtask", prompt: "...", agent: "..."}`
- **ReasoningPart**: `{type: "reasoning", text: "..."}`
- **StepStartPart / StepFinishPart**: Agentic step boundaries with token counts

### GET /session/{sessionID}/message/count

```
Response 200: {"count": 42}
```

### GET /session/{sessionID}/message/{messageID}

Get specific message with parts.

### DELETE /session/{sessionID}/message/{messageID}

Delete a message. Does NOT revert file changes.

### DELETE /session/{sessionID}/message/{messageID}/part/{partID}

Delete a single part from a message. Does NOT revert file changes.

Useful for trimming a session's storage — e.g. when one tool call captured a huge stdout dump (a multi-MB `jj st`, a `find /`, etc.) that bloats the on-disk DB and reload time. The handler is a single SQL `DELETE FROM part WHERE id=? AND session_id=?` and emits a `message.part.removed` event so any connected TUI updates live. Tool parts are atomic — call input, output, and status all live in one row, so deletion can't leave a half-attached tool result for the model to choke on if you resume the session.

```
Response 200: true
Response 404: NotFoundError
```

### PATCH /session/{sessionID}/message/{messageID}/part/{partID}

Replace a part in place. Body is a full `Part` object whose `id`, `messageID`, and `sessionID` must match the path params (mismatch returns a `Part mismatch:` error). Useful for editing tool output, redacting secrets that landed in a part, or fixing a malformed `text` part without rebuilding the surrounding turn.

```
Response 200: Part (the updated part)
Response 404: NotFoundError
```

---

## Prompting

### POST /session/{sessionID}/message

Send a message synchronously (waits for full response).

```json
Body:
{
  "parts": [
    {"type": "text", "text": "Your prompt"},
    {"type": "file", "mime": "image/png", "url": "data:image/png;base64,..."}
  ],
  "model": {
    "providerID": "anthropic",
    "modelID": "claude-sonnet-4-20250514"
  },
  "agent": "build"
}
```

### POST /session/{sessionID}/prompt_async

Send a message asynchronously (returns immediately).

Same body as `/message`. Returns **204** on acceptance.

Use this for fire-and-forget prompts (worker dispatch, feedback relay).

### POST /session/{sessionID}/command

Execute a slash command.

```json
Body:
{
  "command": "legion-worker",
  "arguments": "implement mode for eng-42"
}
```

### POST /session/{sessionID}/shell

Run a shell command in session context.

```json
Body:
{
  "command": "bun test",
  "agent": "build"
}
```

---

## Todos

### GET /session/{sessionID}/todo

Get session todo list.

```
Response 200:
[
  {"content": "Implement feature X", "status": "completed", "priority": "high"},
  {"content": "Write tests", "status": "in_progress", "priority": "high"},
  {"content": "Push changes", "status": "pending", "priority": "medium"}
]
```

Status: `pending`, `in_progress`, `completed`, `cancelled`
Priority: `high`, `medium`, `low`

---

## Permissions & Questions

### GET /permission

List pending permission requests across all sessions.

### POST /permission/{requestID}/reply

Respond to a permission request.

```json
Body:
{
  "reply": "once"  // once | always | reject
}
```

### GET /question

List pending questions across all sessions.

### POST /question/{requestID}/reply

Answer a question.

```json
Body:
{
  "answers": [["Selected option label"]]
}
```

### POST /question/{requestID}/reject

Reject a question.

---

## Workspaces

### GET /experimental/workspace

List all workspaces.

### POST /experimental/workspace

Create a workspace.

```json
Body:
{
  "type": "worktree",
  "branch": "feature-branch",
  "extra": null
}
```

### DELETE /experimental/workspace/{id}

Remove a workspace. ID must match `^wrk.*`.

---

## Worktrees

### POST /experimental/worktree

Create a git worktree.

```json
Body:
{
  "name": "feature-branch",
  "startCommand": "bun install"
}
```

Returns `{name, branch, directory}`.

### GET /experimental/worktree

List all worktree directories. Returns `string[]`.

### DELETE /experimental/worktree

Remove a worktree and delete its branch.

```json
Body:
{
  "directory": "/path/to/worktree"
}
```

### POST /experimental/worktree/reset

Reset a worktree branch to primary default branch.

```json
Body:
{
  "directory": "/path/to/worktree"
}
```

---

## Events (SSE)

### GET /event

Subscribe to project-scoped events via Server-Sent Events.

Key event types for controller use:

| Event | Properties | Use |
|-------|-----------|-----|
| `session.status` | `{sessionID, status}` | Monitor busy/idle |
| `session.idle` | `{sessionID}` | Detect worker completion |
| `message.updated` | `{info: Message}` | New message posted |
| `message.part.updated` | `{part: Part}` | Tool call completed |
| `todo.updated` | `{sessionID, todos[]}` | Worker progress |
| `permission.asked` | `{id, sessionID, permission, patterns}` | Permission needed |
| `question.asked` | `{id, sessionID, questions[]}` | Question pending |
| `session.error` | `{sessionID, error}` | Session error |

---

## Files & Search

### GET /file?path=...

List files and directories at a path.

### GET /file/content?path=...

Read file content.

### GET /file/status

Get git status of all files. Returns `{path, added, removed, status}[]`.

### GET /find?pattern=...

Search file contents (uses ripgrep).

### GET /find/file?query=...

Search for files by name/pattern.

### GET /find/symbol?query=...

Search for workspace symbols (functions, classes) via LSP.

---

## Configuration

### GET /config

Get current configuration.

### PATCH /config

Update configuration.

### GET /config/providers

List configured AI providers and default models.

---

## Providers

### GET /provider

List all available AI providers with connection status.

### GET /provider/auth

Get authentication methods for all providers.

---

## MCP Servers

### GET /mcp

Get status of all MCP servers.

### POST /mcp

Add a new MCP server dynamically.

```json
Body:
{
  "name": "my-server",
  "config": {
    "type": "local",
    "command": ["node", "server.js"]
  }
}
```

### POST /mcp/{name}/connect

Connect an MCP server.

### POST /mcp/{name}/disconnect

Disconnect an MCP server.

---

## PTY (Terminal Sessions)

### GET /pty

List active PTY sessions.

### POST /pty

Create a PTY session.

```json
Body:
{
  "command": "bash",
  "args": [],
  "cwd": "/path",
  "title": "My terminal"
}
```

### GET /pty/{ptyID}/connect

WebSocket connection to interact with PTY in real-time.

---

## Agents & Skills

### GET /agent

List available AI agents. Returns `Agent[]` with name, mode, model, permissions.

### GET /skill

List available skills with content. Returns `{name, description, location, content}[]`.

### GET /command

List available slash commands.

---

## Paths & VCS

### GET /path

Get working directory paths: `{home, state, config, worktree, directory}`.

### GET /vcs

Get version control info: `{branch: "main"}`.

---

## LSP & Formatter

### GET /lsp

Get LSP server status.

### GET /formatter

Get formatter status.
