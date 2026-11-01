# Trilium Notes - Phase 1 Screening

```yaml
tool_name: Trilium Notes
category: PKM / Knowledge Graph
official_url: https://triliumnotes.org/
pricing: open_source
pricing_details: |
  Free and open source under AGPL-3.0 license
  Optional paid hosting available via trilium.cc (third-party)
  Self-hosting is fully free with no feature restrictions
platforms:
  web_ui: yes  # Full web interface via self-hosted server, accessible via browser
  desktop: Windows, macOS, Linux
  mobile: no  # No official mobile app; web PWA access via mobile browser; TriliumDroid (unofficial Android app)
api:
  exists: yes
  type: REST
  read_write: both
open_source: yes
repo_url: https://github.com/TriliumNext/Trilium
last_commit: 2026-01-08  # v0.101.3 released January 8, 2026
github_stars: 34400
screening_result: PASS
fail_reason:

additional_notes: |
  ## Project History
  - Originally created by Zadam (Adam) starting December 25, 2017 (8 years old)
  - Original repo zadam/trilium was transferred to TriliumNext community in 2025
  - Project rebranded back to "Trilium Notes" from "TriliumNext Notes"
  - Active development continues under TriliumNext organization

  ## Architecture
  - Built with TypeScript, Node.js, and Electron for desktop
  - Uses SQLite database for note storage (not plain text files)
  - Single database allows for features like clones (same note in multiple tree locations)
  - Scales well up to 100,000+ notes
  - License: AGPL-3.0

  ## API Details (ETAPI)
  - ETAPI = External/Public REST API, available since v0.50
  - Full OpenAPI specification available in repository
  - Token-based authentication (Options -> ETAPI) or Basic Auth (since v0.56)
  - Endpoints include:
    - /etapi/notes - Note CRUD operations
    - /etapi/notes/{NOTE_ID}/content - Get/set note content
    - /etapi/branches - Branch/tree management
    - /etapi/attributes - Note attribute operations
    - /etapi/attachments - Attachment handling
    - /etapi/search - Note search functionality
    - /etapi/app-info - Application info
  - Python client library: trilium-py (pip install trilium-py)

  ## MCP Server Availability (Critical for AI Agent Orchestration)
  - Multiple MCP server implementations exist:
    - aimbitgmbh/trillium-mcp (npm: @aimbitgmbh/trillium-mcp) - JavaScript/TypeScript
    - pwelty/mcp_trilium - Python implementation
    - tan-yong-sheng/triliumnext-mcp
    - h30190/trilium_mcp
    - radonx-mcp-trilium
  - MCP capabilities include:
    - Full CRUD on notes (create, read, update, delete)
    - Search with Trilium's query syntax
    - Support for archived note inclusion
    - Subtree-specific searches
  - Compatible with Claude Desktop and other MCP clients
  - Safety: Can be set to READ-only mode via PERMISSIONS='READ'

  ## Self-Hosting
  - Docker deployment available: trilium image
  - Web UI accessible via browser at port 8080
  - Server syncs with desktop clients
  - PWA installation supported for mobile-like experience

  ## Knowledge Graph Features
  - Hierarchical note organization (tree structure)
  - Note cloning: same note can appear in multiple tree locations
  - Relations: typed connections between notes
  - Link Map: visualizes incoming/outgoing links for a note
  - Relation Map: manually created diagram of notes and their relations
  - Note Map: auto-generated visualization of note connections
  - Uses jsPlumb library for visual connectivity
  - Supports #mapIncludeRelation and #mapExcludeRelation labels for filtering

  ## Unique Features
  - Day notes: automatic daily journal notes
  - Note hoisting: focus on subtree
  - Full-text search
  - Per-note encryption
  - Custom scripting (JavaScript frontend/backend APIs)
  - Custom widgets (now support Preact/JSX)
  - Code notes with syntax highlighting
  - Task management capabilities

  ## Mobile Access Options
  - Web PWA via mobile browser (touch-optimized mobile frontend)
  - TriliumDroid: unofficial Android app (available via F-Droid/IzzyOnDroid)
  - trilium-sender: Android app for sending notes (write-only)
  - iOS: Apple Shortcuts integration, or web PWA

  ## Recent Updates (v0.101.x)
  - Modernized UI with breadcrumb navigation
  - Back/forward navigation buttons
  - Enhanced zen mode
  - Windows 11 Mica effect support
  - Mermaid diagram improvements

additional_sections:
  agent_integration:
    mcp_servers: 5+ community implementations
    api_completeness: high  # Full CRUD, search, attributes, branches
    real_time_capability: unknown  # No WebSocket mentions found
    authentication: token-based or basic auth (simple to integrate)
    python_client: trilium-py (pip)

  data_model:
    type: hierarchical tree with relations
    storage: SQLite database
    reference_granularity: note-level (with clones for multi-location)
    special_features: clones, relations, attributes, per-note encryption
    visualization: link map, relation map, note map

  sync_capabilities:
    server_sync: yes (desktop clients sync with server)
    encryption: per-note encryption available
    conflict_resolution: built-in sync mechanism

  community:
    documentation: https://docs.triliumnotes.org/
    github_discussions: yes
    awesome_list: github.com/Nriver/awesome-trilium
    third_party_hosting: trilium.cc
```

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://docs.triliumnotes.org/user-guide/advanced-usage/etapi
  auth_model: api_key  # ETAPI token from Options -> ETAPI, also supports Basic Auth (since v0.56)
  rate_limits: |
    - Returns 429 when client IP blacklisted due to too many requests
    - Triggered by excessive requests or failed authentication attempts
    - No documented specific limits (requests/minute, etc.)
  webhooks: no  # No native outgoing webhooks; can create incoming handlers via scripting
  subscriptions: no  # No subscription/push mechanism via API
  mcp_server: community  # Multiple community implementations available
  mcp_server_url: |
    - https://github.com/aimbitgmbh/trillium-mcp (TypeScript, npm: @aimbitgmbh/trillium-mcp)
    - https://github.com/pwelty/mcp_trilium (Python)
    - https://github.com/tan-yong-sheng/triliumnext-mcp (TypeScript)
    - https://github.com/RadonX/mcp-trilium (TypeScript)

data_model:
  node_structure: |
    - Notes are the primary entity, stored in SQLite `notes` table
    - Tree structure maintained via `branches` table (parent-child relationships)
    - Cloning: single note can have multiple branches (appear in multiple tree locations)
    - Blobs store actual content of notes, attachments, and revisions
    - Entities: BNote, BBranch, BAttribute, BRevision, BBlob correspond to SQLite tables
  schema_flexibility: freeform  # Notes can have any content; attributes are flexible key-value pairs
  custom_fields: yes  # Via attributes (labels and relations)
  relations: |
    - Two types of attributes: Labels (#name=value) and Relations (~name=targetNoteId)
    - Labels: key-value metadata attached directly to notes
    - Relations: semantic links connecting notes to other notes
    - Attribute inheritance: inheritable attributes apply to all child notes
    - Tree hierarchy via branches (parent-child)
    - Clones allow same note in multiple tree locations

task_support:
  native_tasks: no  # No built-in task type; Task Manager is a scripting showcase/template
  status_field: no  # Must be implemented via custom attributes (e.g., doneDate label)
  assignee_field: no  # Can be added via custom attributes
  due_date: no  # Can be added via custom attributes (e.g., todoDate, doneDate promoted attributes)
  dependencies: workaround  # Possible via relations between notes, not native task feature
  dependency_description: |
    - Task dependencies not natively supported
    - Can model dependencies via custom relations between notes (e.g., ~blockedBy, ~blocks)
    - Task Manager template uses attributes like todoDate, doneDate, tags, location
    - Requires custom scripting to implement dependency logic
    - Event handlers (runOnAttributeChange) can automate status changes
  query_ready_tasks: no  # Requires custom implementation
    # Workaround: Use attribute search like `#todoDate != '' AND #doneDate = ''`
    # Combined with relation traversal for dependencies: `~blockedBy.#doneDate = ''`
    # Would need custom scripting to properly filter "ready" tasks

query_capabilities:
  simple_filters: yes  # Attribute filters like `#book`, `#author=Tolkien`
  graph_traversal: yes  # Limited; relation traversal in queries supported
  multi_hop_queries: |
    - Supported via dot notation: `~author.relations.son.title = 'Christopher Tolkien'`
    - Can traverse multiple relations in a single query
    - Example: Find notes with author relation to a note that has son relation to target
    - Primarily 2-3 hops practical; complex graph queries not optimized
  query_language: other  # Custom Trilium search syntax (not Datalog/GraphQL/Cypher/SQL)
  full_text_search: yes  # Full-text search on title and content, tokenized by whitespace
  vector_search: no  # No native vector/embedding search (AI features use external LLMs)

scale:
  documented_limits: |
    - Tested up to 100,000-150,000 notes without noticeable slowdown
    - SQLite database stored in single file (~trilium-data/document.db)
    - No documented maximum storage limit (filesystem dependent)
    - Large file imports capped at 250 MiB per file
  concurrent_access: |
    - Single-user application by design
    - Multi-user NOT officially supported (workarounds exist)
    - Sync mechanism supports multiple devices for same user (star topology)
    - Concurrent editing from multiple clients can cause sync conflicts
    - Conflict resolution: last write wins, older version preserved in revisions
  known_performance_issues: |
    - Lagging frontend when editing very large notes (reported issue #3478)
    - Sync can fail with many large files (1GB+ per file)
    - Not recommended to share SQLite database over network drive

hosting:
  hosted_option: yes  # Third-party: trilium.cc (not official)
  hosted_pricing: varies  # Third-party service, not officially maintained
  self_host_complexity: easy  # Docker one-liner deployment
  self_host_requirements: |
    - Docker and Docker Compose (recommended)
    - Official images for AMD64, ARMv7, ARM64/v8
    - Minimum: Linux/Windows/macOS with Docker support
    - Port 8080 (default)
    - Volume mount for ~/trilium-data persistence
    - USER_UID/USER_GID env vars for permissions
    - HTTPS via reverse proxy recommended (nginx, Caddy, etc.)
  data_export: yes  # HTML, Markdown, OPML formats
    # Export formats: HTML (native), Markdown (with some formatting loss), OPML
    # Import formats: Markdown, ENEX (Evernote), HTML
    # trilium-py supports import from Joplin, Obsidian, VNote, Logseq
    # Backup: Automatic database backups, cron schedulable

real_time:
  sync_mechanism: |
    - WebSocket for real-time frontend updates
    - HTTP/WebSocket for client-server sync (star topology)
    - Entity changes tracked in `entity_changes` table
    - Optimistic updates on client with background persistence (SpacedUpdate)
    - Becca (backend) and Froca (frontend) in-memory caches
    - Sync protocol version must match between client and server
  latency: not_documented  # Real-time within same instance; sync intervals not specified
  conflict_resolution: |
    - Last-write-wins strategy based on utcDateModified timestamp
    - If clientEntity.utcDateModified > serverEntity.utcDateModified: client wins
    - Otherwise: server wins, client pulls server version on next sync
    - Older versions preserved in note revisions for recovery
    - Hash verification ensures data integrity after sync
    - Automatic recovery mechanism on hash mismatch

agent_integration:
  mcp_tools_available: |
    ## aimbitgmbh/trillium-mcp (TypeScript - comprehensive):
    Notes:
    - notes_search: Search notes using Trilium query language
    - note_get: Get note metadata and content
    - note_list_children: List all children of a note
    - note_create: Create new note (WRITE)
    - note_overwrite: Replace note content (WRITE)
    - note_delete: Delete a note (WRITE)
    - note_create_revision: Create revision snapshot (WRITE)
    - note_reorder: Reorder note within parent (WRITE)
    - note_reorder_children: Reorder all children (WRITE)
    - note_edit: Surgical find-and-replace (WRITE)
    - note_prepend: Add content at beginning (WRITE)
    - note_append: Add content at end (WRITE)
    - note_grep: Search within note content
    - note_get_lines: Read specific line ranges
    Branches:
    - branches_get: Retrieve branch details
    - branches_create: Place note in tree (WRITE)
    - branches_update: Modify branch properties (WRITE)
    - branches_delete: Remove note from parent (WRITE)
    Attributes:
    - attributes_get: Retrieve attribute details
    - attributes_create: Add label/relation (WRITE)
    - attributes_update: Modify attribute (WRITE)
    - attributes_delete: Remove attribute (WRITE)

    ## pwelty/mcp_trilium (Python):
    - search_trilium_notes: Full-text search with filters
    - get_recent_trilium_notes: Retrieve recently modified notes
    - get_trilium_note: Read note by ID
    - create_trilium_note: Create new notes
    - update_trilium_note: Edit existing notes
    - delete_trilium_note: Remove notes
    - get_trilium_note_tree: Browse hierarchical structures
    - get_trilium_note_attributes: View labels/relations
    - add_trilium_note_attribute: Add custom attributes
    - export_trilium_note: Export in various formats
    - backup_trilium_note: Create note backups
    - get_trilium_app_info: Application stats and version
  langchain_integration: no  # No dedicated LangChain integration found
  llamaindex_integration: no  # No dedicated LlamaIndex integration found
  other_integrations: |
    - trilium-py: Python client library (pip install trilium-py)
    - TriliumNext has built-in AI features supporting OpenAI, Anthropic, Ollama
    - Internal agentic tool calling and embeddings (within Trilium)
    - Custom scripting API for automation (JavaScript backend/frontend)
    - Event handlers: runOnNoteChange, runOnNoteContentChange, runOnAttributeChange
    - Incoming webhook-like capability via custom request handlers
```

### Phase 2 Research Sources

- [ETAPI Documentation](https://docs.triliumnotes.org/user-guide/advanced-usage/etapi)
- [API Reference](https://docs.triliumnotes.org/user-guide/advanced-usage/etapi/api-reference)
- [TriliumNext GitHub Repository](https://github.com/TriliumNext/Trilium)
- [Search Functionality](https://docs.triliumnotes.org/user-guide/concepts/navigation/search)
- [Synchronization Guide](https://docs.triliumnotes.org/user-guide/setup/synchronization)
- [Attributes Documentation](https://docs.triliumnotes.org/user-guide/advanced-usage/attributes)
- [Task Manager Showcase](https://docs.triliumnotes.org/user-guide/advanced-usage/advanced-showcases/task-manager)
- [Docker Installation](https://docs.triliumnotes.org/user-guide/setup/server/installation/docker)
- [Import & Export](https://docs.triliumnotes.org/user-guide/concepts/import-export)
- [MCP Server: pwelty/mcp_trilium](https://github.com/pwelty/mcp_trilium)
- [MCP Server: aimbitgmbh/trillium-mcp](https://github.com/aimbitgmbh/trillium-mcp)
- [MCP Server: tan-yong-sheng/triliumnext-mcp](https://github.com/tan-yong-sheng/triliumnext-mcp)
