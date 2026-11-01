# Mem - Phase 1 Screening

```yaml
tool_name: Mem
category: PKM / Knowledge Graph
official_url: https://mem.ai (also https://get.mem.ai)
pricing: freemium
pricing_details: |
  Free: 25 notes/month, 25 chat messages/month
  Pro: $12/month (unlimited usage, AI features, API access)
  Teams: $15-20/user/month (collaboration features, shared AI)
  Enterprise: Custom pricing
  Annual billing saves ~20%
  Student/academic/non-profit discounts available
platforms:
  web_ui: yes
  desktop: yes (macOS and Windows)
  mobile: iOS only (no Android - officially stated "not yet available, no timeline")
api:
  exists: yes
  type: REST
  read_write: write_only (currently)
  notes: |
    Current API (v0/v2): Write-only - create mems, append to mems, batch create (up to 100)
    Endpoints: https://api.mem.ai/v0/mems (create), https://api.mem.ai/v0/mems/:memId/append
    Authentication: Bearer token (API key from Settings > Integrations)
    Read/outflow APIs: Planned but not yet released (as of initial API launch)
open_source: no
repo_url: N/A
last_commit: N/A
github_stars: N/A
screening_result: PASS
fail_reason: N/A

additional_notes: |
  ## Product Overview
  Mem is an AI-powered note-taking and knowledge management app, described as an "AI Thought Partner."
  Backed by OpenAI's fund. Mem 2.0 launched in spring 2025 with significant improvements.

  ## Key Features
  - Voice Mode: Transcribe thoughts/meetings into organized notes
  - Deep Search: AI-powered retrieval even from vague queries
  - Collections: Flexible grouping (notes can be in multiple collections)
  - Mem Chat: AI trained on user's own notes for extraction/summarization
  - Related Notes: Automatic suggestion of relevant notes
  - Agentic Chrome Extension: One-click webpage capture

  ## API Limitations (Important)
  The API is currently **write-only**:
  - Can CREATE notes (mems)
  - Can APPEND to existing notes
  - Can BATCH CREATE (up to 100 at once)
  - CANNOT read/search/retrieve notes via API (outflow APIs are "planned")

  This is a significant limitation for AI agent orchestration - agents can push data
  INTO Mem but cannot programmatically retrieve or search existing knowledge.

  ## MCP Server Availability
  A Mem MCP server exists via Composio (https://mcp.composio.dev/mem) with 5 tools:
  - Create Collection
  - Create Note
  - Delete Collection
  - Delete Note
  - Read Note (retrieves content by note ID)

  The Composio MCP integration appears to provide read capabilities that may not be
  available via the standard REST API, potentially through different authentication
  or internal APIs.

  Note: Mem0 (mem-zero) is a DIFFERENT product - an open-source memory layer for AI agents.
  Do not confuse Mem.ai with Mem0/mem0ai on GitHub.

  ## Agent Orchestration Assessment
  Strengths:
  - MCP server available with read/write capabilities
  - Good for capturing agent outputs and meeting notes
  - AI-powered search within the app

  Weaknesses:
  - REST API is write-only (no programmatic search/retrieval)
  - No Android app
  - Closed source
  - Knowledge stays siloed in Mem's ecosystem

  ## Real-time Capabilities
  - Offline support with sync
  - No real-time collaboration mentioned
  - No webhooks or event streaming documented

additional_sections:
  integrations:
    zapier: yes
    make: yes
    native_integrations: Chrome extension, SMS/Telegram/WhatsApp via "Text to Mem"

  data_portability:
    export: Unknown (not documented in research)
    import: Batch API supports importing from other platforms

  ai_features:
    built_in_ai: yes
    ai_provider: OpenAI (backed by OpenAI fund)
    features:
      - AI-powered search
      - Note summarization
      - Content drafting
      - Automatic organization
      - Voice transcription
```

## Sources

- [Mem Official Website](https://mem.ai)
- [Mem Pricing](https://get.mem.ai/pricing)
- [Mem API Documentation](https://docs.mem.ai)
- [Mem Help Center - API](https://help.mem.ai/features/api)
- [Introducing the Mem API](https://newsletter.mem.ai/p/introducing-the-mem-api-e8b)
- [Mem MCP via Composio](https://mcp.composio.dev/mem)
- [Android App Support](https://help.mem.ai/article/83-is-there-an-android-app)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://docs.mem.ai
  auth_model: api_key  # Bearer token authentication
  rate_limits: |
    Per minute: 100 requests, 200 complexity tokens
    Per day: 4,000 requests, 8,000 complexity tokens
    /v2/mem-it endpoint: 40 complexity tokens (5 calls/min max)
    Throttling: leaky bucket algorithm
    429 status code with Retry-After header when exceeded
  webhooks: no
  subscriptions: no  # No real-time event subscriptions via API
  mcp_server: community  # Via Composio and BurtTheCoder/mcp-mem.ai
  mcp_server_url: |
    - https://mcp.composio.dev/mem (Composio - 5 tools)
    - https://github.com/BurtTheCoder/mcp-mem.ai (Community - 6 tools)

data_model:
  node_structure: |
    - Notes ("mems"): Primary content unit, markdown-based
    - Collections: Flexible grouping system (replaces folders/tags)
    - Notes can belong to multiple collections simultaneously
    - Bidirectional links between notes via @ mentions
    - AI automatically surfaces related notes and connections
  schema_flexibility: freeform  # Markdown content, no structured schema
  custom_fields: no  # No custom metadata fields beyond content
  relations: |
    - Collections: notes can be in multiple collections
    - Bidirectional links: @ mentions create note-to-note links
    - AI-inferred relations: "Related Notes" and "Heads Up" surfaces connections
    - No explicit typed relationships between entities

task_support:
  native_tasks: yes  # Built-in task/checkbox support
  status_field: yes  # Checkbox checked/unchecked state
  assignee_field: no  # No assignee support (personal/team notes, not project management)
  due_date: yes  # Tasks can be snoozed/scheduled to future dates
  dependencies: no
  dependency_description: |
    No native task dependencies. Tasks are simple checkboxes within notes.
    No blocking/blocked-by relationships between tasks.
    Tasks are aggregated in a central "Tasks" view from across all notes.
  query_ready_tasks: no  # Cannot query "unblocked tasks" since no dependencies exist

query_capabilities:
  simple_filters: yes  # Filter by date, creator, editor, content type
  graph_traversal: no  # No explicit graph queries
  multi_hop_queries: |
    Not supported. No way to query "notes linked to notes in collection X"
    or traverse relationships programmatically.
    AI search may surface related content but not via explicit graph queries.
  query_language: none  # Natural language search only (AI-powered)
  full_text_search: yes  # "Deep Search" with AI-enhanced matching
  vector_search: yes  # AI-powered semantic search ("Deep Search results")

scale:
  documented_limits: |
    Free tier: 25 notes/month, 25 chat messages/month
    Pro/paid: Unlimited notes
    API batch create: up to 100 notes per request
    No documented max storage limits
  concurrent_access: |
    Team plans support collaboration
    Server-side intelligent merge for concurrent edits
    Real-time conflict resolution for shared notes/collections
  known_performance_issues: None documented

hosting:
  hosted_option: yes  # Cloud-only SaaS
  hosted_pricing: |
    Free: $0 (limited)
    Personal/Pro: $8-12/month
    Teams: $15-20/user/month
    Enterprise: Custom pricing
    20% discount for annual billing
  self_host_complexity: n/a  # No self-hosting available
  self_host_requirements: n/a  # Cloud-only service
  data_export: yes  # JSON format with markdown content

real_time:
  sync_mechanism: |
    Local-first, event-driven architecture
    Changes saved locally first, then synced to server
    Server distributes updates to all devices
    Works offline with automatic sync when reconnected
  latency: Not documented specifically
  conflict_resolution: |
    Server-side intelligent merge for concurrent edits
    Real-time conflict resolution for team collaboration
    No explicit CRDT mentioned but merge behavior described

agent_integration:
  mcp_tools_available: |
    Via Composio (5 tools):
    - create_collection: Create new collection with title, content, parentId
    - create_note: Create note with content, memId for idempotency
    - read_note: Retrieve note content/metadata by ID
    - delete_note: Remove note by ID
    - delete_collection: Remove collection by ID

    Via BurtTheCoder/mcp-mem.ai (6 tools):
    - mem_it: AI-powered content processing/organization
    - create_note: Structured markdown note creation
    - read_note: Retrieve note by ID
    - delete_note: Remove note
    - create_collection: Organize related notes
    - delete_collection: Remove collection
  langchain_integration: no  # No direct Mem.ai LangChain integration
  llamaindex_integration: no  # No direct Mem.ai LlamaIndex integration
  other_integrations: |
    - Zapier: yes
    - Make (Integromat): yes
    - Chrome extension
    - SMS/Telegram/WhatsApp via "Text to Mem"
    Note: Mem0 (different product) has LangChain/LlamaIndex integrations

# Additional Phase 2 Notes

api_capabilities_detail: |
  ## v2 API Endpoints (Current)
  Collections:
  - POST /v2/collections - Create collection
  - GET /v2/collections/:id - Read collection
  - DELETE /v2/collections/:id - Delete collection
  - GET /v2/collections - List collections
  - POST /v2/collections/search - Search collections (query parameter)

  Notes:
  - POST /v2/notes - Create note (content, memId, source, createdAt, isRead)
  - GET /v2/notes/:id - Read note
  - DELETE /v2/notes/:id - Delete note
  - GET /v2/notes - List notes
  - POST /v2/notes/search - Search notes

  Special:
  - POST /v2/mem-it - AI-powered "remember anything" (40 complexity tokens)

  ## v0 API Endpoints (Legacy)
  - POST /v0/mems - Create mem
  - POST /v0/mems/:memId/append - Append to existing mem

key_limitations: |
  1. No graph query language - cannot traverse note relationships programmatically
  2. No webhooks or event streaming - must poll for changes
  3. No task dependencies - basic checkbox tasks only
  4. No self-hosting option - cloud-only SaaS
  5. Limited custom metadata - freeform markdown only
  6. No Android app
  7. Rate limits may constrain high-volume agent usage (4,000 req/day)

strengths_for_agents: |
  1. MCP servers available (Composio + community)
  2. AI-powered semantic search ("Deep Search")
  3. Full CRUD via API (v2 includes read operations)
  4. Automatic AI organization reduces need for manual structuring
  5. Local-first sync with offline support
  6. Team collaboration with conflict resolution
  7. Bidirectional note linking

recommendation: |
  Mem.ai is suitable for capturing agent outputs and building a searchable
  knowledge base with AI-enhanced retrieval. However, it lacks:
  - Structured graph queries for complex relationship traversal
  - Task dependencies for workflow modeling
  - Real-time event streaming for reactive agents

  Best fit: Personal/team knowledge capture with AI-powered search
  Poor fit: Complex project management, graph-based reasoning, real-time agent coordination
```

## Phase 2 Sources

- [Mem API Documentation](https://docs.mem.ai)
- [Mem API Authentication](https://docs.mem.ai/api-reference/overview/authentication)
- [Search Collections Endpoint](https://docs.mem.ai/api-reference/collections/search-collections)
- [Mem MCP via Composio](https://mcp.composio.dev/mem)
- [mcp-mem.ai GitHub](https://github.com/BurtTheCoder/mcp-mem.ai)
- [Mem Pricing](https://get.mem.ai/pricing)
- [Managing Tasks in Mem.ai](https://www.ctnet.co.uk/managing-tasks-in-mem-ai/)
- [Mem Task View](https://get.mem.ai/help-and-support/manage-your-tasks-with-task-view)
- [Mem 2.0 Sync Architecture](https://get.mem.ai/blog/mem-2-0-dev-update-sync)
- [Bidirectional Links in Mem](https://blog.maximizeyouroutput.com/unleashing-creativity-with-bidirectional-links-in-mem-the-mad-libs-method-clq9qp7ox1964741wr37rk0bn73/)
- [Mem.ai Data Export](https://www.amplenote.com/plugins/uHhuZS9DTwF5tPEy5gr2ARYD)
