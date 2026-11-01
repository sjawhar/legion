# AFFiNE - Phase 1 Screening

```yaml
tool_name: AFFiNE
category: PKM / Knowledge Graph
official_url: https://affine.pro/
pricing: freemium
pricing_details: |
  Free tier: Unlimited local workspaces (MIT license), 10GB cloud storage, 3 workspace members, 7-day version history
  Pro: $6.75/month (billed annually) - 100GB storage, 10 members, 30-day history, real-time collaboration
  Team: $10/seat/month (10+ seats) - 100GB + 20GB/seat, unlimited members, admin roles
  Believer (Lifetime): $499.99 one-time - 1TB storage, lifetime personal use
  AI Add-on: $8.90/month - AI writing, summarization, drawing, presentations
platforms:
  web_ui: yes
  desktop: yes  # Windows, macOS, Linux (Electron-based)
  mobile: both  # iOS and Android released December 2025
api:
  exists: yes
  type: GraphQL  # Primary API; REST endpoints for streaming/binary/SSE
  read_write: both
open_source: yes
repo_url: https://github.com/toeverything/AFFiNE
last_commit: January 2026  # Repository shows active development with 66 open PRs
github_stars: 62400  # 62.4k stars
screening_result: PASS
fail_reason: null

additional_notes: |
  ## Core Architecture
  - Local-first, offline-capable architecture with CRDT-based sync (using Yjs)
  - OctoBase: Rust-based data engine for local-first yet collaborative storage
  - BlockSuite: Open-source collaborative editor framework
  - Treats documents and whiteboards as equal citizens ("hyper-fused platform")

  ## API Details
  - GraphQL endpoint: https://app.affine.pro/graphql (with playground)
  - Schema includes: User, Workspace, Document, Comment, Copilot, Subscription types
  - REST endpoints for AI streaming responses and binary data
  - WebSocket-based real-time collaboration
  - Self-hosting supported with full API access

  ## MCP Server Available
  - Community MCP server: https://github.com/DAWNCR0W/affine-mcp-server
  - 59 stars, MIT license, v1.2.2 (September 2025)
  - 30+ tools for workspace/document operations
  - WebSocket-based document editing (CRDT updates)
  - Capabilities: workspace CRUD, document list/search/publish, comments, version history, blob storage
  - Authentication: API token, cookie, or email/password
  - Transport: stdio only (Claude Desktop / Codex compatible)

  ## Real-Time Capabilities
  - CRDT-driven P2P synchronization
  - Conflict-free merging after offline periods
  - Socket.IO for WebSocket communication
  - Naturally compatible with end-to-end encryption (E2EE)
  - Supports multi-platform native sync

  ## Agent Orchestration Relevance
  - Strong: GraphQL API with comprehensive schema
  - Strong: MCP server exists with 30+ tools
  - Strong: CRDT architecture enables safe concurrent edits
  - Strong: Self-hostable for full control
  - Moderate: Documentation still maturing (community discussions mention gaps)
  - Note: Mobile app still has some editing limitations

  ## Technology Stack
  - TypeScript/React frontend
  - NestJS backend server
  - Yjs for CRDT
  - Electron for desktop apps
  - Jotai for state management
  - y-octo (Rust implementation of CRDT)

  ## Licensing
  - Most code under MIT license
  - Backend server (packages/backend/server) has separate "EE" license
  - Community Edition free to self-host under MIT

additional_sections:
  mcp_integration:
    available: yes
    server_name: affine-mcp-server
    maintainer: DAWNCR0W (community)
    install: npm i -g affine-mcp-server
    tools_count: 30+
    key_capabilities:
      - Workspace CRUD operations
      - Document list/search/publish/revoke
      - WebSocket-based document editing
      - Comment management
      - Version history access
      - Blob storage management
      - User profile management

  collaboration_model:
    type: CRDT (Yjs-based)
    offline_support: yes
    conflict_resolution: automatic
    e2ee_compatible: yes
    real_time: yes (Socket.IO/WebSocket)

  self_hosting:
    supported: yes
    license: MIT (Community Edition)
    docker_available: yes
    documentation: https://docs.affine.pro/self-host-affine
```

## Sources

- [AFFiNE Official Website](https://affine.pro/)
- [AFFiNE GitHub Repository](https://github.com/toeverything/AFFiNE)
- [AFFiNE Documentation](https://docs.affine.pro/)
- [AFFiNE Pricing](https://affine.pro/pricing)
- [AFFiNE MCP Server](https://github.com/DAWNCR0W/affine-mcp-server)
- [GraphQL Playground](https://app.affine.pro/graphql)
- [AFFiNE CRDT Architecture Blog](https://affine.pro/blog/why-affine-chose-crdt-over-ot-to-build-a-collaborative-editor)
- [AFFiNE on Google Play](https://play.google.com/store/apps/details?id=app.affine.pro)

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://docs.affine.pro/ # API docs are sparse; GraphQL schema available at /graphql playground
  auth_model: api_key / session_cookie / email_password  # Three options, token preferred
  rate_limits: not documented  # No public rate limit documentation found
  webhooks: no  # No webhook API found in documentation or community
  subscriptions: no  # No GraphQL subscriptions documented
  mcp_server: community  # DAWNCR0W/affine-mcp-server
  mcp_server_url: https://github.com/DAWNCR0W/affine-mcp-server

data_model:
  node_structure: |
    Block-based architecture where everything is a "block":
    - Blocks are abstract data units with unique IDs and flavours (types)
    - Block flavours: paragraph, heading (h1-h6), code, quote, list, to-do, divider, etc.
    - Blocks can be transformed between types while preserving metadata
    - Pages and Edgeless canvases are containers (Databases) of blocks
    - Blocks can be referenced, embedded, and linked across views
    - Each block has props that determine display (e.g., text, type)
  schema_flexibility: dynamic  # Blocks can transform between types; custom props per flavour
  custom_fields: yes  # Block props are extensible per flavour type
  relations: |
    - Blocks can be linked via internal links (treated same as external links)
    - Documents can be organized in workspaces
    - Comments attached to documents
    - No native graph/relation system between documents (feature requested by community)

task_support:
  native_tasks: yes  # To-do blocks, kanban views, checklist templates
  status_field: yes  # To-do blocks have checked/unchecked state; Kanban has columns
  assignee_field: no  # No native assignee field; workaround via text/tags
  due_date: no  # No native due date field; workaround via database views or text
  dependencies: workaround
  dependency_description: |
    No native task dependencies. Workarounds include:
    - Using Gantt chart templates with manual dependency visualization
    - Database views with custom columns for dependency tracking
    - Text-based linking between related tasks
    - Community has requested relational database links feature
  query_ready_tasks: no  # Cannot natively query "tasks with no blockers"

query_capabilities:
  simple_filters: yes  # Document search by keyword via API
  graph_traversal: no  # No graph database; blocks are hierarchical within documents
  multi_hop_queries: no  # Cannot query "tasks blocked by tasks owned by X"
  query_language: graphql  # Primary API is GraphQL
  full_text_search: yes  # search_docs tool in MCP, basic keyword search
  vector_search: partial  # pgvector extension supported for AI features when self-hosted

scale:
  documented_limits: |
    Cloud Storage:
    - Free: 10GB storage, 10MB file upload limit
    - Pro: 100GB storage, 100MB file upload limit
    - Enterprise: 500MB file upload limit
    Workspace Members:
    - Free: 3 members
    - Pro: 10 members
    - Team: unlimited
    Performance benchmarks:
    - ~100MB Postgres growth per 1k docs (avg 1k words/doc)
    - ~10GB blob storage growth per 1k blobs
    - 1GB peak memory for merging doc with 10k modifications
  concurrent_access: yes  # CRDT enables conflict-free concurrent edits
  known_performance_issues: |
    - Large documents (10k+ modifications) require significant memory for merging
    - File system performance significantly impacts overall performance
    - Self-hosted instances may inherit 10MB upload limit without configuration

hosting:
  hosted_option: yes  # app.affine.pro
  hosted_pricing: |
    Free: $0 - 10GB storage, 3 members
    Pro: $6.75/month (annual) - 100GB storage, 10 members
    Team: $10/seat/month (10+ seats) - unlimited members
    Believer: $499.99 one-time - 1TB lifetime
  self_host_complexity: moderate
  self_host_requirements: |
    - Docker Compose (recommended method)
    - PostgreSQL 16 with pgvector extension (for AI features)
    - Redis 6.x or 7.x (required for sync/caching)
    - Minimum: 4 CPU cores, 2-4GB RAM
    - ~1.5GB disk for installation
    - Optional: Prometheus for monitoring (~200MB additional)
    - S3-compatible storage or Cloudflare R2 for blob storage
  data_export: yes / PDF, HTML, Markdown, .affine format
  # Note: Full workspace export limited to .affine format; individual pages export to PDF/HTML/Markdown

real_time:
  sync_mechanism: |
    - CRDT (Yjs/y-octo) for conflict-free data synchronization
    - WebSocket via Socket.IO for real-time communication
    - Local storage: SQLite (desktop) or IndexedDB (web)
    - NBStore class coordinates local-remote sync
    - Redis PubSub for horizontal scaling in multi-server deployments
  latency: not documented  # Depends on network and server configuration
  conflict_resolution: |
    CRDT-based automatic conflict resolution:
    - Operations are commutative (same result regardless of order)
    - No central source of truth required
    - Offline edits merge automatically when reconnecting
    - Yjs ensures eventual consistency across all clients

agent_integration:
  mcp_tools_available: |
    36 tools via affine-mcp-server (v1.2.2):

    Workspace Management:
    - list_workspaces, get_workspace, create_workspace
    - update_workspace, delete_workspace

    Document Operations:
    - list_docs, get_doc, search_docs, recent_docs
    - publish_doc, revoke_doc
    - create_doc, append_paragraph, delete_doc (WebSocket)
    - apply_doc_updates (CRDT)

    Comments:
    - list_comments, create_comment, update_comment
    - delete_comment, resolve_comment

    Version Control:
    - list_histories, recover_doc

    User/Auth:
    - current_user, sign_in, update_profile, update_settings
    - send_verify_email, change_password, send_password_reset
    - delete_account

    Access Tokens:
    - list_access_tokens, generate_access_token, revoke_access_token

    Blob Storage:
    - upload_blob, delete_blob, cleanup_blobs

    Notifications:
    - list_notifications, read_notification, read_all_notifications
  langchain_integration: no  # No official or community LangChain integration found
  llamaindex_integration: no  # No official or community LlamaIndex integration found
  other_integrations: |
    - Alternative MCP server: Paperfeed/affine-mcp (lighter implementation)
    - n8n workflow automation (via HTTP/GraphQL)
    - Native AI features via AI Add-on ($8.90/month)
```

## Phase 2 Sources

- [AFFiNE MCP Server GitHub](https://github.com/DAWNCR0W/affine-mcp-server)
- [AFFiNE MCP Server Schema - Glama](https://glama.ai/mcp/servers/@DAWNCR0W/affine-mcp-server/schema)
- [AFFiNE Self-Hosting Requirements](https://docs.affine.pro/self-host-affine/install/requirements)
- [AFFiNE Docker Compose Setup](https://docs.affine.pro/self-host-affine/install/docker-compose-recommend)
- [AFFiNE Blocks Documentation](https://docs.affine.pro/core-concepts/elements-of-affine/blocks)
- [AFFiNE Block Model - BlockSuite](https://docs.affine.pro/blocksuite-wip/store/block-model)
- [AFFiNE Architecture - DeepWiki](https://deepwiki.com/toeverything/AFFiNE)
- [Yjs CRDT Documentation](https://docs.yjs.dev/)
- [AFFiNE API Discussion - GitHub](https://github.com/toeverything/AFFiNE/discussions/6052)
- [AFFiNE Community Feature Requests](https://community.affine.pro/c/feature-requests)
