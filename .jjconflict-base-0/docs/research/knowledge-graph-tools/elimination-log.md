# Phase 1 Elimination Log

## Pass Criteria
- PASS if: Has API (read or write) AND has Web UI
- FAIL if: Missing API OR missing Web UI

---

## Failed Tools (5)

### Logseq
- **Category:** PKM / Knowledge Graph
- **Fail Reason:** No web UI
- **Details:** Has robust local HTTP API with multiple MCP servers, but web version is deprecated/demo only. Desktop-focused architecture requires local app running.

### Obsidian
- **Category:** PKM / Knowledge Graph
- **Fail Reason:** No web UI
- **Details:** Desktop/mobile only. Has REST API via community plugin and multiple MCP servers, but requires local desktop app running. Not suitable for cloud-based agent orchestration.

### Tana
- **Category:** PKM / Knowledge Graph
- **Fail Reason:** Write-only API
- **Details:** API confirmed to be write-only ("Input API"). Cannot query, search, or retrieve existing data. Read API is on roadmap but not implemented. Unsuitable for bidirectional agent workflows.

### RemNote
- **Category:** PKM / Knowledge Graph
- **Fail Reason:** No external API
- **Details:** Only has Plugin SDK that runs inside the app. Backend API was deprecated. Cannot access programmatically from outside the application.

### Anytype
- **Category:** PKM / Knowledge Graph
- **Fail Reason:** No web UI
- **Details:** Local-first architecture requires app installation. Has local REST API + official MCP server, but no browser-based access. Interesting for local agent use cases but doesn't meet web UI requirement.

---

## Passed Tools with Caveats (3)

### Reflect
- **Caveat:** API is effectively write-only due to E2E encryption
- **Details:** Servers cannot decrypt note contents, so API can only write/append. Can read metadata (links, books, user info) but not note content. Poor fit for agent orchestration requiring content retrieval.

### Mem
- **Caveat:** REST API is write-only (read APIs planned but not released)
- **Details:** Official API can only create/append notes. "Outflow APIs" (read capabilities) are planned but not yet available. MCP via Composio may provide read workaround.

### Capacities
- **Caveat:** Limited write capabilities, single-user only
- **Details:** Beta API can only create weblinks and daily note entries. Cannot create arbitrary typed objects. No collaboration features - designed for individual use only.
