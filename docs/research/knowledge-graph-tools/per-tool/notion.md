# Notion - Phase 1 Screening

```yaml
tool_name: Notion
category: PKM / Knowledge Graph
official_url: https://www.notion.com
pricing: freemium
pricing_details: |
  Free: $0/member/month - Limited blocks for teams, unlimited for individuals, 5MB file uploads, 7-day history, 10 guests
  Plus: $10/member/month - Unlimited blocks, unlimited file uploads, 30-day history, 100 guests, basic integrations
  Business: $20/member/month - Granular permissions, private teamspaces, SAML SSO, premium integrations, built-in AI (GPT-4.1, Claude 3.7 Sonnet)
  Enterprise: Custom pricing - Zero data retention with LLM providers, SCIM, advanced security, audit logs, DLP/SIEM integrations
  Note: 20% discount for annual billing
platforms:
  web_ui: yes
  desktop: Mac, Windows (no official Linux support)
  mobile: both (iOS 17.0+, Android)
api:
  exists: yes
  type: REST
  read_write: both
open_source: no
repo_url: https://github.com/makenotion/notion-mcp-server  # MCP server is open source
last_commit: 2025-03 (MCP server created)
github_stars: 3800  # MCP server repo
screening_result: PASS
fail_reason:

additional_notes: |
  ## MCP Server (Critical for AI Agents)
  - Official MCP server available: https://github.com/makenotion/notion-mcp-server
  - MIT licensed, 3.8k GitHub stars
  - Hosted MCP server available for OAuth-based setup (no API tokens needed)
  - 21 tools available for AI agent interaction
  - Supports both STDIO and Streamable HTTP transport modes
  - Markdown-optimized API designed for token-efficient LLM interactions
  - Compatible with Claude Desktop, Claude.ai, Cursor, ChatGPT, and other MCP clients

  ## API Details
  - REST API (not GraphQL - third-party wrappers exist)
  - Latest version: 2025-09-03 with multi-source database support
  - API access available on all tiers including Free
  - Full CRUD operations on pages, databases, blocks, users
  - Webhooks and automations available on paid tiers

  ## AI Features (2026)
  - Notion Agent built-in (Business/Enterprise)
  - Model choice: GPT-4.1, Claude 3.7 Sonnet, Gemini 3 Pro
  - Mobile AI note transcription with background processing
  - AI summarization, action item extraction

  ## Real-time & Offline
  - Offline mode available (Plus tier and above)
  - Auto-download of Recents and Favorites for offline use
  - No native real-time collaboration API (web-based real-time in app)

  ## Performance (2026)
  - Windows pages open 27% faster
  - Mac desktop +11% faster
  - Mobile pages loading 15% faster

  ## Enterprise Features
  - MCP activity tracking in audit logs
  - Query multiple databases via MCP
  - Control over which external AI tools can connect (coming soon)
  - Zero data retention with LLM providers

additional_sections:
  mcp_integration:
    hosted_server: yes
    self_hosted_option: yes
    tools_count: 21
    oauth_support: yes
    token_optimized: yes

  knowledge_graph_capabilities:
    linked_databases: yes
    bi_directional_links: yes
    relation_properties: yes
    rollup_properties: yes
    synced_blocks: yes
    graph_view: no  # No native graph visualization

  ai_agent_readiness: high
  integration_ecosystem: extensive  # Zapier, Make, n8n, etc.
```

## Sources
- [Notion Pricing](https://www.notion.com/pricing)
- [Notion API Documentation](https://developers.notion.com)
- [Notion MCP Server (GitHub)](https://github.com/makenotion/notion-mcp-server)
- [Notion MCP Documentation](https://developers.notion.com/docs/mcp)
- [Notion Desktop App](https://www.notion.com/desktop)
- [Notion Mobile App](https://www.notion.com/mobile)
- [Notion Help Center](https://www.notion.com/help)
- [Notion Releases - January 2026](https://www.notion.com/releases/2026-01-20)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://developers.notion.com
  auth_model: oauth  # OAuth 2.0 for user-authorized integrations, bearer token for internal integrations
  rate_limits: |
    - 3 requests/second average per integration (180 requests/minute)
    - Bursts beyond average allowed
    - HTTP 429 with Retry-After header when exceeded
    - MCP tools may have additional stricter limits
  webhooks: yes  # Real-time notifications for page/database changes
  subscriptions: yes  # Webhook subscriptions for page.created, page.updated, database changes, etc.
  mcp_server: official  # makenotion/notion-mcp-server
  mcp_server_url: https://github.com/makenotion/notion-mcp-server

data_model:
  node_structure: |
    Hierarchical block-based structure:
    - Workspaces contain teamspaces and pages
    - Pages contain blocks (text, databases, embeds, etc.)
    - Databases are special pages with structured properties
    - Blocks can be nested (max depth varies by block type)
    - Everything is a "block" internally (pages are also blocks)
  schema_flexibility: dynamic  # Properties can be added/modified on databases at any time
  custom_fields: yes  # Rich property types: text, number, select, multi-select, date, person, files, checkbox, URL, email, phone, formula, relation, rollup, created_time, created_by, last_edited_time, last_edited_by, status
  relations: |
    - Relation properties: Link entries between databases (one-way or two-way)
    - Rollup properties: Aggregate data from related entries (count, sum, percent, earliest/latest date, etc.)
    - Self-relations supported (database to itself)
    - Max 100 relations per property
    - Synced blocks: Mirror content across pages

task_support:
  native_tasks: yes  # Built-in task databases with status, assignee, dates
  status_field: yes  # Native status property with customizable stages
  assignee_field: yes  # Person property for assignment
  due_date: yes  # Date properties with start/end, reminders
  dependencies: yes  # Native dependencies feature in timeline view
  dependency_description: |
    - "Blocking" and "Blocked by" relations between tasks
    - Visual arrows in timeline view
    - Automatic date shifting options when dependencies change
    - Three modes: shift on overlap, maintain time gap, or no auto-shift
    - Weekend avoidance option for shifted dates
  query_ready_tasks: workaround  # Filter by status + check blocking relations, but no native "unblocked" filter

query_capabilities:
  simple_filters: yes  # Filter by any property type with various operators
  graph_traversal: no  # Cannot traverse relation chains in a single query
  multi_hop_queries: |
    Not natively supported. To find "tasks blocked by tasks owned by X":
    1. Query tasks owned by X
    2. For each result, query tasks that have blocking relation to those
    Requires multiple API calls and client-side assembly
  query_language: none  # REST API with JSON filter objects (compound AND/OR supported)
  full_text_search: limited  # Search endpoint finds by title only; content search requires in-app or Notion AI
  vector_search: no  # Not exposed via API (Enterprise has internal semantic search via Notion AI)

scale:
  documented_limits: |
    - 10,000 rows per database
    - 50 columns per database
    - 1,000 blocks per page
    - 100 elements per block array
    - 100 related pages per relation property
    - 50KB max database schema size
    - 2.5MB max per page (all properties)
    - 1.5MB max total property structure per database
    - 5MB file upload (free), unlimited (paid)
    - 500KB max API payload size
    - 1000 blocks max per API request
    - 100 results per paginated response
  concurrent_access: |
    - No limit on concurrent viewers/editors per page
    - Real-time collaboration for small-medium teams works well
    - 50+ concurrent users in same database may experience sync delays
    - 100+ concurrent users may see temporary performance degradation
    - Enterprise-grade multi-agent support with explicit permissions
  known_performance_issues: |
    - Complex formulas/rollups slow database loading
    - Filtering/sorting by formula/rollup properties is slower
    - Databases with "several dozen thousand pages" recommended to split
    - Large relation chains impact performance

hosting:
  hosted_option: yes  # Notion is SaaS-only
  hosted_pricing: |
    Free: $0 (limited blocks for teams, 5MB uploads, 7-day history)
    Plus: $10/member/month (unlimited blocks, 30-day history)
    Business: $20/member/month (granular permissions, SAML SSO, Notion AI)
    Enterprise: Custom (zero data retention, SCIM, audit logs)
  self_host_complexity: n/a  # No self-hosted option
  self_host_requirements: n/a
  data_export: yes  # Markdown, CSV, HTML, PDF (Business+); JSON via API

real_time:
  sync_mechanism: |
    - WebSocket-based real-time sync in web/desktop apps
    - Last-Write-Wins (LWW) for concurrent edits to same block
    - Webhooks for server-to-server notifications (events, not full content)
    - Notion sponsored Peritext CRDT research for future improvements
  latency: |
    - Sub-second for most operations in-app
    - Webhooks may have "delays up to several seconds"
    - Event aggregation during rapid updates
  conflict_resolution: |
    - Last-Write-Wins at block level (paragraph-sized units)
    - Concurrent edits to same block: one edit preserved, other discarded
    - Adding/moving blocks is intention-preserving
    - No true CRDT for text yet (researching via Peritext)
    - Offline edits may lose data if conflicting

agent_integration:
  mcp_tools_available: |
    16 official MCP tools:
    - notion-search: Search workspace and connected tools
    - notion-fetch: Retrieve page/database content by URL
    - notion-create-pages: Create one or more pages
    - notion-update-page: Modify page properties/content
    - notion-move-pages: Relocate pages/databases
    - notion-duplicate-page: Clone a page (async)
    - notion-create-database: Create new database with schema
    - notion-update-data-source: Modify database properties
    - notion-query-data-sources: Query multiple databases (Enterprise + AI)
    - notion-query-database-view: Query using view filters (Business+)
    - notion-create-comment: Add page comments
    - notion-get-comments: List page comments
    - notion-get-teams: List teamspaces
    - notion-get-users: List workspace users
    - notion-get-user: Get user by ID
    - notion-get-self: Get bot/connection info
  langchain_integration: yes  # NotionDBLoader for document loading, database querying
  llamaindex_integration: yes  # llama-index-readers-notion for loading, Notion Tool for agent actions
  other_integrations: |
    - Composio: Full Notion toolkit for crewAI, LangChain, LlamaIndex, OpenAI
    - n8n: Native Notion nodes for automation workflows
    - Zapier: 5000+ app integrations
    - Make (Integromat): Visual automation builder
    - Pipedream: Developer-friendly automation
    - Third-party SDKs: Python (notion-sdk-py), JavaScript (@notionhq/client), Go, Ruby, etc.
```

## Phase 2 Sources
- [Notion API Request Limits](https://developers.notion.com/reference/request-limits)
- [Notion API Filter Database](https://developers.notion.com/reference/post-database-query-filter)
- [Notion Webhooks Documentation](https://developers.notion.com/reference/webhooks)
- [Notion MCP Supported Tools](https://developers.notion.com/docs/mcp-supported-tools)
- [Notion Relations & Rollups](https://www.notion.com/help/relations-and-rollups)
- [Notion Sub-items & Dependencies](https://www.notion.com/help/tasks-and-dependencies)
- [Notion Database Performance](https://www.notion.com/help/optimize-database-load-times-and-performance)
- [Notion Export Content](https://www.notion.com/help/export-your-content)
- [Notion GDPR](https://www.notion.com/help/gdpr-at-notion)
- [LangChain Notion Loader](https://python.langchain.com/docs/integrations/document_loaders/notion/)
- [LlamaIndex Notion Reader](https://docs.llamaindex.ai/en/stable/examples/data_connectors/NotionDemo/)
- [Composio Notion Integration](https://composio.dev/tools/notion/all)
- [Notion Hosted MCP Server Blog](https://www.notion.com/blog/notions-hosted-mcp-server-an-inside-look)
- [Peritext CRDT Research](https://www.inkandswitch.com/peritext/)
