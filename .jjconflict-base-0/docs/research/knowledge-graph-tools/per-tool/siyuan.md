# SiYuan - Phase 1 Screening

```yaml
tool_name: SiYuan
category: PKM / Knowledge Graph
official_url: https://b3log.org/siyuan/
pricing: freemium
pricing_details: |
  Free tier: $0 (lifetime) - All basic features, local use only
  PRO tier: $64 (one-time) - Adds S3/WebDAV third-party sync
  Subscription: $148 (one-time, lifetime) - Adds official cloud sync (8GB), cloud inbox, asset hosting
  Note: Core features are free; paid tiers primarily add sync capabilities
platforms:
  web_ui: yes  # Via Docker/self-hosted deployment on port 6806
  desktop: Windows, macOS, Linux
  mobile: both  # iOS (App Store), Android (Google Play, F-Droid), HarmonyOS
api:
  exists: yes
  type: REST
  read_write: both
open_source: yes
repo_url: https://github.com/siyuan-note/siyuan
last_commit: 2026-01  # Active development, recent commits on master
github_stars: 41000
screening_result: PASS
fail_reason:

additional_notes: |
  ## Architecture
  - Written in TypeScript (frontend) and Go (backend/kernel)
  - License: AGPL-3.0
  - Local-first architecture with optional cloud sync
  - Block-level content model (similar to Notion)
  - Supports Markdown WYSIWYG editing

  ## API Details
  - REST API on http://127.0.0.1:6806 (or self-hosted URL)
  - Token-based authentication via header: Authorization: Token xxx
  - All endpoints use POST method with JSON body
  - Comprehensive operations: notebooks, documents, blocks, attributes, assets, SQL queries, templates, files, export
  - SQL query capability allows flexible data retrieval

  ## MCP Server Availability (Critical for AI Agent Orchestration)
  - Multiple MCP server implementations exist:
    - onigeya/siyuan-mcp-server (most popular)
    - GALIAIS/siyuan-mcp-server
    - porkll/siyuan-mcp
  - MCP capabilities include:
    - Full CRUD on notebooks, documents, blocks
    - SQL queries and full-text search
    - File/asset management
    - OCR and multi-format export (via Pandoc)
    - System notifications
  - Compatible with Claude Desktop, Cursor IDE, and other MCP clients

  ## Self-Hosting
  - Docker deployment available: b3log/siyuan
  - Web UI accessible via browser at port 6806
  - Access controlled via authorization code (accessAuthCode)
  - Limitations in Docker: no desktop/mobile app connections, no PDF/Word export, no Markdown import

  ## Knowledge Graph Features
  - Block-level references (fine-grained linking)
  - Bidirectional links
  - Graph view for visualizing connections
  - Supports million-word documents

  ## Plugin Ecosystem
  - Plugin system called "Petal"
  - Community marketplace "Bazaar"
  - Community SDK available: @siyuan-community/siyuan-sdk (npm)

  ## Sync Options
  - Official cloud sync (paid, 8GB)
  - S3-compatible storage (e.g., Cloudflare R2 free tier)
  - WebDAV
  - Community "Better Sync" plugin (P2P, free)
  - End-to-end encryption supported

  ## Privacy & Security
  - Privacy-first design
  - Complete offline capability
  - End-to-end encrypted sync
  - Self-hosted option keeps data on-premise

additional_sections:
  agent_integration:
    mcp_servers: 3+ community implementations
    api_completeness: high  # Full CRUD, SQL queries, file ops
    real_time_capability: unknown  # No WebSocket/subscription mentions found
    authentication: token-based (simple, easy to integrate)

  data_model:
    type: block-based
    reference_granularity: block-level
    query_language: SQL
    export_formats: Markdown, PDF, HTML, Word (desktop only)

  community:
    forum: https://liuyun.io (LiuYun)
    documentation: Official + community developer docs
    sdk: npm @siyuan-community/siyuan-sdk
```

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://github.com/siyuan-note/siyuan/blob/master/API.md
  auth_model: api_key  # Token-based auth via "Authorization: Token xxx" header
  rate_limits: none documented  # No rate limits specified; self-hosted so effectively unlimited
  webhooks: no  # No webhook support; push notifications only for in-app alerts
  subscriptions: no  # No real-time subscription API
  mcp_server: community  # Multiple community implementations
  mcp_server_url: https://github.com/onigeya/siyuan-mcp-server  # Most popular/mature implementation

data_model:
  node_structure: |
    Block-based architecture where every piece of content is a discrete, referenceable block with:
    - ID: Unique 21-character identifier (14-digit timestamp + 7 random chars)
    - Box: Notebook identifier
    - Path: File system path to containing document
    - RootID: Parent document ID
    - ParentID: Reference to parent block (enables tree structure)
    - Type: Block category (NodeHeading, NodeParagraph, NodeList, etc.)
    - Content: Text payload
    - IAL: Inline Attribute List for metadata as key-value pairs {: key="value"}
    Documents stored as .sy files containing JSON-serialized block trees.
    SQLite databases provide indexing: siyuan.db (main), blocktree.db (FTS5 search), history.db
  schema_flexibility: dynamic  # IAL allows arbitrary key-value metadata on any block
  custom_fields: yes  # Via IAL (Inline Attribute List) and custom-* attributes
  relations: |
    - Block references: [[blockID]] creates explicit links tracked in refs table
    - refs table maintains: source block ID, target block ID, reference type
    - Bidirectional links: Backlinks auto-generated via refs index
    - Virtual references: Automatic linking based on keyword matching
    - Parent-child: ParentID field creates hierarchical tree structure
    - Attribute Views: Database-like relations between attribute view entries (15+ column types including relation, rollup)

task_support:
  native_tasks: yes  # Markdown task syntax "- [ ]" supported
  status_field: no  # Only checkbox completion, no custom statuses natively
  assignee_field: no  # Not native; possible via custom IAL attributes
  due_date: no  # Not native; feature request was closed as "not planned"
  dependencies: workaround  # Not native; requires custom attributes or plugins
  dependency_description: |
    No native task dependencies. Workarounds include:
    - Using custom IAL attributes (e.g., {: blocked-by="blockID"})
    - Using Attribute View relations to link task entries
    - Third-party plugins like "Schedule Manager" for kanban-style workflows
    - SQL queries can join on custom attributes to find dependency chains
  query_ready_tasks: yes  # Via SQL: SELECT * FROM blocks WHERE markdown LIKE '%[ ]%' AND subtype='t' AND type='i'

query_capabilities:
  simple_filters: yes  # SQL WHERE clauses on any block field
  graph_traversal: yes  # Via refs table JOINs and recursive queries
  multi_hop_queries: |
    Possible via SQL JOINs:
    - Single-hop: SELECT * FROM blocks WHERE id IN (SELECT block_id FROM refs WHERE def_block_id='xyz')
    - Multi-hop requires nested subqueries or CTEs (SQLite supports recursive CTEs)
    - Example 2-hop: Find blocks referencing blocks that reference X
    - Depth limited by query complexity rather than API constraints
    - fb2p (reference redirection) handles nested container blocks automatically
  query_language: sql  # Full SQLite with FTS5 extension
  full_text_search: yes  # FTS5 via blocks_fts table; API endpoint /api/search/fullTextSearch
  vector_search: no  # No native vector/semantic search; would require plugin or external integration

scale:
  documented_limits: |
    - Cloud sync: 8GB storage per paid subscriber
    - Local storage: Limited only by disk space
    - Documents: Supports "million-word documents" with dynamic loading
    - Embed recursion: Max depth of 7 to prevent infinite loops
    - No documented limits on block count, notebook count, or concurrent connections
  concurrent_access: |
    - Single-user by design (local-first)
    - Multi-device sync via cloud or third-party services (not real-time collaborative)
    - Docker deployment supports browser access (no concurrent editing)
    - "Better Sync" plugin offers experimental WebSocket-based sync with conflict tracking
  known_performance_issues: |
    - Initial indexing can be slow for large workspaces
    - SQLite temp database rebuilt on startup
    - Third-party sync services may corrupt data (not officially supported)

hosting:
  hosted_option: no  # No official hosted/SaaS version
  hosted_pricing: n/a
  self_host_complexity: easy  # Single Docker container
  self_host_requirements: |
    - Docker: b3log/siyuan image
    - Port 6806 exposed
    - Workspace volume mount recommended
    - PUID/PGID for permission management
    - No specific RAM/CPU minimums documented (runs on Synology NAS)
    - accessAuthCode for browser access control
  data_export: yes  # JSON (.sy), Markdown, PDF, Word, HTML, zip archives

real_time:
  sync_mechanism: |
    - Official cloud sync: Periodic sync (not real-time), end-to-end encrypted
    - Multi-kernel perception: After sync completes, notifies other kernels via WebSocket
    - "Better Sync" plugin: Experimental WebSocket-based near-instant sync
    - No native real-time collaborative editing (like Google Docs)
  latency: |
    - Cloud sync: Batch-based, not sub-second
    - "Better Sync" plugin: Near-instant for small changes
  conflict_resolution: |
    - Official sync: Last-write-wins with history snapshots for recovery
    - "Better Sync" plugin: Creates conflict files when same file modified on multiple devices
    - Sync locking prevents simultaneous sync operations
    - History-based protection adds files to history before syncing
    - .siyuan folder sync handled separately to avoid metadata corruption

agent_integration:
  mcp_tools_available: |
    40+ operations via onigeya/siyuan-mcp-server:
    - Notebook: lsNotebooks, createNotebook, openNotebook, closeNotebook, renameNotebook, removeNotebook, getNotebookConf, setNotebookConf
    - Document: createDocWithMd, renameDoc, removeDoc, moveDocs, getHPathByPath, getHPathByID
    - Block: insertBlock, updateBlock, deleteBlock, moveBlock, getBlockKramdown
    - Attribute: setBlockAttrs, getBlockAttrs
    - File: getFile, putFile, removeFile, readDir
    - Assets: uploadAssets
    - Query: sql, block, fullTextSearch
    - Export: exportNotebook, exportDoc, pandoc conversion
    - System: getVersion, getBootProgress, getCurrentTime
    - Notification: pushMsg, pushErrMsg
    - Template: renderTemplate, renderSprig
  langchain_integration: no  # No official integration; would need custom tool wrapper
  llamaindex_integration: no  # No official integration; would need custom data loader
  other_integrations: |
    - Claude Desktop: Via MCP server configuration
    - Cursor IDE: Via MCP server
    - Community SDK: npm @siyuan-community/siyuan-sdk
    - Plugin API: "Petal" system for extending SiYuan
    - Any MCP-compatible client can use the siyuan-mcp-server
```
