# OpenCode Serve API — Complete Endpoint Reference

Base URL: `http://127.0.0.1:13381`

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
