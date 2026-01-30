# Reflect - Phase 1 Screening

```yaml
tool_name: Reflect
category: PKM / Knowledge Graph
official_url: https://reflect.app
pricing: paid_only
pricing_details: |
  No free tier available (14-day free trial only)
  Standard: $10/month or $120/year (some sources cite $15/month)
  All plans include:
  - Unlimited AI access (GPT-4 powered)
  - Calendar integration (Google Calendar, Outlook)
  - End-to-end encryption
  - Mobile + Desktop apps
  - Voice transcription (Whisper)
  Note: No student or non-profit discounts available
platforms:
  web_ui: yes
  desktop: Mac, Windows (no native Linux - web app works on Linux)
  mobile: iOS only (no native Android app - web app works)
api:
  exists: yes
  type: REST
  read_write: limited  # See notes - write-only for note content due to E2E encryption
open_source: no
repo_url: https://github.com/team-reflect  # Organization with supporting tools, not main app
last_commit: N/A  # Main app is proprietary
github_stars: N/A  # Main app is proprietary
screening_result: PASS
fail_reason:

additional_notes: |
  ## API Capabilities (Critical Limitation)
  The API is fundamentally LIMITED due to end-to-end encryption architecture:

  ### Read Operations (Available)
  - GET /graphs - List all graphs (workspaces)
  - GET /graphs/{graphId}/books - Fetch bookmarked books (Kindle integration)
  - GET /graphs/{graphId}/links - Retrieve bookmarked links with highlights
  - GET /users/me - Current user info (uid, email, name, graph_ids)

  ### Write Operations (Available)
  - POST /graphs/{graphId}/links - Create new link entry
  - PUT /graphs/{graphId}/daily-notes - Append to daily note
  - POST /graphs/{graphId}/notes - Create new note (subject + content_markdown)

  ### Critical Constraint
  **Note CONTENT is NOT readable via API** - Due to E2E encryption, servers cannot
  decrypt note contents. The API is append-only/write-only for actual note text.
  This is a fundamental architectural constraint, not a feature gap.

  ## Authentication
  - OAuth 2.0 with PKCE support
  - Scopes: `read:graph`, `write:graph`
  - Token endpoint: https://reflect.app/api/oauth/token
  - Authorization: https://reflect.app/oauth

  ## MCP Server
  No official MCP server found in Model Context Protocol repositories.
  No community MCP server discovered during research.
  Given the E2E encryption limiting read access, an MCP server would have
  very limited utility for AI agents (write-only capabilities).

  ## AI Features (Built-in)
  - GPT-4 integration for AI chat with notes
  - Whisper for voice transcription
  - Gemini support (2M token context) for chatting with notes
  - Custom AI prompts available

  ## Knowledge Graph Features
  - Backlinked/networked notes (bi-directional linking)
  - Graph of ideas visualization
  - No folders - flat structure with backlinks only
  - Limited formatting (headers, bold, italic, bullets, links only)

  ## Integrations
  - Zapier integration (append-to-daily-note)
  - Readwise integration
  - Google Calendar & Outlook calendar sync
  - Chrome/Safari browser extensions
  - Kindle highlights import

  ## Security Model
  - End-to-end encryption (true zero-knowledge)
  - If password forgotten, data is unrecoverable
  - No backdoor or recovery option by design

additional_sections:
  api_limitations:
    note_content_readable: no  # E2E encryption prevents server-side reading
    note_content_writable: yes  # Append-only
    metadata_readable: yes  # Links, books, user info
    graph_structure_readable: no  # Cannot read note graph/backlinks via API

  mcp_integration:
    official_server: no
    community_server: no
    feasibility: low  # E2E encryption severely limits read capabilities

  knowledge_graph_capabilities:
    bi_directional_links: yes
    graph_visualization: yes
    folders: no
    tags: yes
    daily_notes: yes

  ai_agent_readiness: low  # Write-only API limits orchestration potential
  encryption_model: end_to_end  # Zero-knowledge architecture
```

## Sources
- [Reflect Official Website](https://reflect.app)
- [Reflect API Documentation](https://reflect.academy/api)
- [Reflect OpenAPI Specification](https://openpm.ai/apis/reflect)
- [Reflect API Announcement](https://reflect.app/blog/reflect-update-api)
- [Reflect GitHub Organization](https://github.com/team-reflect)
- [Reflect on NoteApps.info](https://noteapps.info/apps/reflect)
- [Reflect Review on ToolGuide](https://toolguide.io/en/tool/reflect-notes/)
- [Reflect Platform Details](https://webcatalog.io/en/apps/reflect)
- [MCP Server Registry](https://github.com/modelcontextprotocol/servers)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://reflect.academy/api
  auth_model: oauth  # OAuth 2.0 with PKCE support
  auth_details: |
    - Authorization URL: https://reflect.app/oauth
    - Token URL: https://reflect.app/api/oauth/token
    - Scopes: read:graph, write:graph
    - PKCE supported for client-side apps
  rate_limits: undocumented  # No rate limit information found in official docs
  webhooks: no  # No webhook support documented
  subscriptions: no  # No real-time subscription API
  mcp_server: community  # Zapier MCP only (not native)
  mcp_server_url: https://zapier.com/mcp/reflect
  mcp_details: |
    Zapier provides MCP bridge with 3 actions:
    - Append to Daily Note
    - Create Link
    - Create Note
    Cost: 2 Zapier tasks per MCP tool call
    Limitation: Write-only due to E2E encryption

data_model:
  node_structure: |
    - Notes: Primary content containers with subject, content (markdown), pinned status
    - Daily Notes: Special notes tied to dates
    - Links: Bookmarked URLs with title, description, highlights
    - Books: Kindle integration with ASIN, title, authors, notes
    - Tags: Hierarchical categorization (#tag/subtag format)
    - Backlinks: Bi-directional associations between notes via [[notation]]
  schema_flexibility: fixed  # Predefined note structure, no custom schemas
  custom_fields: no  # No user-defined fields beyond tags
  relations: |
    - Backlinks: Bi-directional links via [[note title]] syntax
    - Tags: Categorical grouping via #tag syntax
    - Daily Note associations: Tasks and notes linked to dates
    - No explicit relationship types or properties on links

task_support:
  native_tasks: yes  # Tasks beta feature available
  status_field: limited  # Only complete/incomplete, no custom statuses
  assignee_field: no  # No assignee support
  due_date: workaround  # Via Daily Note backlinks (schedule to future dates)
  dependencies: no  # No dependency support
  dependency_description: |
    Reflect does not support task dependencies. Tasks are simple checkboxes
    that can be scheduled to dates via Daily Note backlinks. Their stated goal
    is to cover 80% of use cases, recommending dedicated task managers (Things,
    etc.) for advanced needs like recurring tasks and dependencies.
  query_ready_tasks: no  # Cannot query "unblocked tasks" as dependencies don't exist
  task_details: |
    - Creation: /Task command, + shortcut, or convert bullet
    - Categories: Current, Overdue, Upcoming (based on Daily Note date)
    - Operations: Check off, schedule, convert to checklist, archive
    - Aggregation: Tasks tab shows all tasks across notes

query_capabilities:
  simple_filters: yes  # Date, backlink, tag, pinned filters
  graph_traversal: no  # No programmatic graph traversal
  multi_hop_queries: |
    Not supported via API. The API cannot read note content due to E2E encryption,
    so graph queries are impossible programmatically. In-app, filters exist but
    no multi-hop query capability (e.g., "notes linked to notes tagged X").
  query_language: none  # No query language, filter UI only
  full_text_search: yes  # Full-text search with OCR for images/PDFs
  vector_search: yes  # Client-side semantic embeddings for "Similar Notes"
  search_details: |
    - Full-text search with exact and quoted phrase matching
    - OCR for images and PDFs
    - Semantic/vector search via client-side embeddings
    - AI chat can query notes with filter context
    - Filters: dates, backlinks, tags, pinned status
    - Access: Command+K hotkey

scale:
  documented_limits: |
    No explicit limits documented for:
    - Maximum notes
    - Storage capacity
    - Note size
    AI processing limits:
    - Free trial: 10,000 characters/day
    - Paid: 64,000 characters/day (bypassable with own OpenAI key)
  concurrent_access: |
    Single-user product. No multi-user collaboration features beyond
    single-click web publishing for sharing. No real-time collaboration.
  known_performance_issues: |
    - Limited to opening one note in main window + one in slideover
    - No splits, tabs, or saved workspaces for multi-note viewing
    - Slideover closes when browsing notes

hosting:
  hosted_option: yes  # Cloud-only SaaS
  hosted_pricing: $10-15/month or $120/year (single plan)
  self_host_complexity: n/a  # No self-hosting option
  self_host_requirements: n/a  # Proprietary cloud-only
  data_export: yes  # Markdown, HTML, JSON formats
  export_details: |
    - Formats: Markdown, HTML, JSON
    - Includes images as separate files
    - Limitation: Export all notes only, cannot export single note
    - Import from: Apple Notes, Evernote, Markdown, Roam, Workflowy
    - Import limitation: Single folder only, no recursive, no embedded media

real_time:
  sync_mechanism: |
    YJS-based CRDT sync engine (Reflect 2.0+). Real-time sync across devices
    with intelligent offline change merging.
  latency: |
    "Instant" sync claimed across macOS, iOS, and web. No specific latency
    metrics documented.
  conflict_resolution: |
    YJS CRDT handles concurrent edits automatically. Offline changes merged
    intelligently when reconnecting. Visual indicator shows when notes have
    unsynced offline changes.
  encryption_note: |
    XChaCha20-Poly1305 encryption. Zero-knowledge architecture means servers
    never see decrypted content. Recovery kit provided at signup.

agent_integration:
  mcp_tools_available: |
    Via Zapier MCP only:
    - append_to_daily_note: Add text to daily note with date/list options
    - create_link: Create bookmarked link entry
    - create_note: Create note with subject and markdown content
    CRITICAL LIMITATION: Read operations unavailable due to E2E encryption
  langchain_integration: no  # No official or community integration found
  llamaindex_integration: no  # No official or community integration found
  other_integrations: |
    - Zapier: Append to daily note action
    - Readwise: Reading highlights sync
    - Calendar: Google Calendar, Outlook sync
    - Browser extensions: Chrome, Safari
    - Kindle: Book highlights import
    - Pipedream: Basic API wrapper

evaluation_summary:
  strengths:
    - Strong end-to-end encryption (true zero-knowledge)
    - Semantic search with client-side vector embeddings
    - YJS-based CRDT sync handles offline gracefully
    - Built-in AI features (GPT-4, Whisper, Gemini)
    - Clean bi-directional linking UX
  weaknesses:
    - API is write-only due to E2E encryption (critical for agents)
    - No task dependencies or advanced task management
    - Single-user only, no collaboration
    - No webhooks or subscriptions
    - No self-hosting option
    - Limited query capabilities (no graph traversal)
  agent_readiness: low
  agent_readiness_rationale: |
    The E2E encryption architecture fundamentally prevents AI agents from reading
    note content via API. Agents can only write/append to notes, making Reflect
    unsuitable for orchestration workflows that require reading and processing
    existing knowledge. Built-in AI features work because they run client-side.
```

## Phase 2 Sources
- [Reflect API Documentation](https://reflect.academy/api)
- [Reflect OpenAPI Spec](https://openpm.ai/apis/reflect)
- [Reflect Tasks Beta](https://reflect.academy/tasks-beta)
- [Reflect Advanced Search](https://reflect.app/blog/ai-search)
- [Reflect Security](https://reflect.academy/security-and-encryption)
- [Reflect Import/Export](https://reflect.academy/import-export-backups)
- [Zapier Reflect MCP](https://zapier.com/mcp/reflect)
- [Reflect Changelog](https://reflect.app/changelog)
- [NoteApps.info Review](https://noteapps.info/apps/reflect)
