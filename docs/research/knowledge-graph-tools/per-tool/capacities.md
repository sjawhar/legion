# Capacities

```yaml
tool_name: Capacities
category: PKM / Knowledge Graph
official_url: https://capacities.io
pricing: freemium
pricing_details: |
  Free tier: Core features, always free
  Pro: $9.99/month (billed annually)
  Believer: $12.49/month (billed annually) - same features as Pro plus early beta access
  14-day free trial available, no credit card required
platforms:
  web_ui: yes
  desktop: yes  # macOS (M series + Intel), Windows, Linux (x64 + Arm)
  mobile: both  # iOS and Android; tablet apps for iPad and Android available for Believer tier
api:
  exists: yes
  type: REST
  read_write: both
  details: |
    Beta API with Bearer token authentication (OAuth 2.0 RFC 6750)
    Rate limit: 120 requests per 60-second window (per user per endpoint)
    OpenAPI 3.0 specification available
    Endpoints:
      - GET /spaces (read) - list user's spaces
      - GET /space-info (read) - get structures and property definitions
      - POST /search (read, deprecated Jan 2026) - full-text search
      - POST /lookup (read) - title-matching search
      - POST /save-weblink (write) - save URLs as objects
      - POST /save-to-daily-note (write) - append to daily notes
open_source: no
repo_url: https://github.com/capacities  # Organization exists but app is proprietary
last_commit: N/A  # Closed source
github_stars: N/A  # Closed source
screening_result: PASS
fail_reason:

additional_notes: |
  ## Overview
  Capacities is a personal knowledge management tool branded as "A studio for your mind."
  It uses an object-based approach rather than traditional files - notes are organized
  as typed objects (books, people, ideas, meetings, etc.) with properties and relationships.

  ## Knowledge Graph Features
  - Bidirectional linking with @mentions or [[wiki-links]]
  - Contextual backlinks showing the full context (parent/child blocks)
  - Block-based linking and embedding
  - Per-object graph view (no global graph view)
  - Unlinked mentions detection (Pro feature)
  - Tags with dedicated tag pages
  - Sorting by number of backlinks

  ## AI Features
  - Built-in AI assistant for interacting with notes
  - Dynamically interact with any object
  - Ask questions based on content
  - Spark new ideas

  ## MCP Server Availability
  Multiple community MCP implementations exist:
  - jem-computer/capacities-mcp (GitHub) - full API integration
  - Capacities Bridge - connects to Claude Desktop, IDEs, Genspark
  - CapacitiesMCP by Inconceivable Labs

  MCP capabilities include:
  - Search across all spaces (full-text and title-based)
  - Browse spaces, structures, and collections
  - Create structured note templates (meeting, research, task-list)
  - Smart analysis for knowledge gaps
  - Built-in rate limiting

  ## Collaboration
  NONE - Capacities is explicitly designed for individual use only.
  No collaborative editing, workspace sharing, or team features planned.
  This is a deliberate design choice, not a limitation.

  ## Sync & Offline
  - Offline-first architecture
  - Automatic cloud sync when online
  - All notes stored locally and synced
  - Calendar sync is NOT real-time (up to 30 seconds delay)

  ## API Limitations
  - API is in Beta
  - Many expected REST endpoints not yet available
  - Limited write operations (only weblinks and daily notes)
  - No endpoint for creating arbitrary objects or modifying existing content

  ## Integrations
  - Calendar integration (Pro)
  - Readwise highlights sync (Pro)
  - Raycast integration (Pro, Mac only)
  - Web Highlights browser extension (Pro)
  - X-Callback-URLs for iOS/macOS automation

  ## Pro-Only Features
  - Calendar integration
  - Smart queries (object, search, tag queries)
  - AI features
  - Task actions to external managers
  - Formulas in tables
  - Unlinked mentions
  - Unlimited file uploads (100 MB/file)
  - Public API access
  - Raycast + Web Highlights integrations

additional_sections:
  agent_orchestration_considerations: |
    Strengths:
    - REST API with OpenAPI spec enables programmatic access
    - Multiple MCP servers available for AI agent integration
    - Object-based structure could map well to agent workflows
    - Good for individual knowledge capture and retrieval

    Limitations:
    - API is Beta with limited write capabilities
    - Cannot create arbitrary objects via API (only weblinks, daily notes)
    - No collaboration means no multi-agent shared workspace
    - No real-time sync for calendar events
    - Rate limits (120 req/60s) may constrain high-frequency agent operations

    Best suited for:
    - Personal research assistants
    - Knowledge capture from web browsing
    - Daily journaling and note aggregation
    - Single-user knowledge retrieval workflows
```

## Sources

- [Capacities Official Website](https://capacities.io)
- [Capacities Documentation](https://docs.capacities.io)
- [Capacities API Documentation](https://docs.capacities.io/developer/api)
- [Capacities API Reference](https://api.capacities.io/docs)
- [Capacities Pro](https://capacities.io/pro)
- [Capacities Pricing](https://capacities.io/pricing)
- [Capacities Collaboration FAQ](https://docs.capacities.io/more/collaboration)
- [Capacities MCP by Jem Gold (GitHub)](https://github.com/jem-computer/capacities-mcp)
- [Capacities MCP on PulseMCP](https://www.pulsemcp.com/servers/jemgold-capacities)

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://docs.capacities.io/developer/api
  api_reference_url: https://api.capacities.io/docs
  auth_model: bearer_token  # OAuth 2.0 RFC 6750, token obtained from desktop app Settings
  rate_limits: |
    Per-user, per-endpoint limits:
    - GET /spaces: 5 req/60s
    - GET /space-info: 5 req/60s
    - POST /search: 120 req/60s (deprecated Jan 2026)
    - POST /lookup: 120 req/60s
    - POST /save-weblink: 10 req/60s
    - POST /save-to-daily-note: 5 req/60s
    Rate limit headers follow IETF draft standard (RateLimit-Remaining)
  webhooks: outgoing_only  # Can send tasks to external services, no incoming webhooks
  subscriptions: no  # No real-time event subscriptions or notifications API
  mcp_server: community
  mcp_server_url: https://github.com/jem-computer/capacities-mcp

data_model:
  node_structure: |
    Object-based architecture where everything is a typed object:
    - Objects have: title, properties, blocks (content area)
    - Built-in types: Page, Task, Tag, DailyNote, MediaImage, MediaPDF, MediaVideo, MediaWebResource
    - Custom object types can be created by users (e.g., Book, Person, Project)
    - Spaces contain multiple object types, each with defined structures
    - Collections group objects within a type
  schema_flexibility: dynamic  # Users can create custom object types and add properties
  custom_fields: yes  # Properties can be added per object type (text, number, checkbox, date, labels, object references)
  relations: |
    - Bidirectional links via @mentions or [[wiki-links]]
    - Object properties: typed links to other object types (e.g., Author property on Book links to Person)
    - Two-way sync: property links can auto-update in both directions
    - Backlinks tracked automatically with full context
    - Tags as a special object type for cross-cutting categorization

task_support:
  native_tasks: yes  # Basic object type, opt-in feature
  status_field: yes  # Customizable statuses with labels and icons
  assignee_field: no  # Single-user app, no assignees needed
  due_date: yes  # Schedulable with calendar integration
  dependencies: no
  dependency_description: |
    No native task dependencies. Capacities explicitly states it is "not a task manager"
    and provides lightweight task management only. For dependency tracking, users are
    directed to use Task Actions to send tasks to external managers (Todoist, TickTick, etc.)
  query_ready_tasks: workaround  # Can use queries to filter tasks by status, but no "unblocked" concept

query_capabilities:
  simple_filters: yes  # Filter by object type, collections, tags, properties
  graph_traversal: limited  # Local graph view shows 1-2 hops; backlink filtering in queries
  multi_hop_queries: |
    Limited. Can filter by backlinked objects (one hop), but no true multi-hop graph queries.
    Cannot query "find all tasks blocked by tasks owned by X" - no dependency concept exists.
    Variable queries allow embedding dynamic filters based on current object's properties.
  query_language: none  # Visual query builder with filters, no text-based query language
  full_text_search: yes  # Searches titles, descriptions, properties, tags, and content
  vector_search: no  # No semantic/embedding-based search

scale:
  documented_limits: |
    - No limits on notes, objects, or spaces
    - Media upload limits on free tier (Pro: unlimited, 100 MB/file)
    - Text content: 200,000 characters per API write operation
    - Tags: 30 max per weblink via API
    - Title: 500 chars, description: 1,000 chars via API
  concurrent_access: single_user  # No collaboration features by design
  known_performance_issues: |
    - Offline-first architecture may require conflict resolution on reconnect
    - PDF rendering can be memory-intensive on mobile
    - Large spaces may show sync warnings if storage is limited

hosting:
  hosted_option: yes  # Cloud-hosted only, no self-host option
  hosted_pricing: |
    Free: Core features, media upload limits
    Pro: $9.99/month (annual) - full API access, unlimited uploads
    Believer: $12.49/month (annual) - same as Pro + beta features
  self_host_complexity: n/a  # No self-hosting available
  self_host_requirements: n/a
  data_export: yes  # Markdown + CSV + media files in human/machine-readable format

real_time:
  sync_mechanism: |
    Offline-first with cloud sync:
    - All content stored locally first
    - Automatic sync when online
    - Manual conflict resolution via popup when version conflicts detected
    - No CRDT - uses last-write-wins with user choice for conflicts
  latency: |
    - Calendar sync: up to 30 seconds delay
    - Note sync: near real-time when online
    - No documented latency guarantees
  conflict_resolution: |
    Manual resolution required. When local and server versions differ:
    - Popup shows both versions
    - User must choose which to keep
    - Unchosen version is permanently deleted
    - No automatic merge capability

agent_integration:
  mcp_tools_available: |
    Via jem-computer/capacities-mcp:
    - capacities_list_spaces: Get all personal spaces
    - capacities_get_space_info: Fetch structures/collections for a space
    - capacities_search: Full-text or title search across spaces
    - capacities_save_weblink: Store URLs with metadata and tags
    - capacities_save_to_daily_note: Append markdown to daily notes

    Via inconceivablelabs/capacitiesMCP:
    - All above plus structured note templates (meeting, research, task-list)
    - Smart analysis for knowledge gaps
    - Built-in rate limiting
  langchain_integration: no  # No official or community LangChain integration found
  llamaindex_integration: no  # No official or community LlamaIndex integration found
  other_integrations: |
    - X-Callback-URLs for iOS/macOS automation
    - Task Actions: Todoist, TickTick, Things, Apple Reminders, Google Tasks, Microsoft To Do
    - Readwise highlights sync
    - Raycast integration (Mac, Pro)
    - Web Highlights browser extension
    - Calendar sync (Google, Apple, Outlook)
    - Generic webhook support for task export
```

## Phase 2 Sources

- [Capacities API Documentation](https://docs.capacities.io/developer/api)
- [Capacities API Reference (Scalar)](https://api.capacities.io/docs)
- [Capacities Object Types](https://docs.capacities.io/reference/content-types)
- [Capacities Properties](https://docs.capacities.io/reference/properties)
- [Capacities Object Properties (Relations)](https://docs.capacities.io/reference/object-properties)
- [Capacities Queries](https://docs.capacities.io/reference/queries)
- [Capacities Task Management](https://docs.capacities.io/reference/task-management)
- [Capacities Task Actions](https://docs.capacities.io/reference/task-actions)
- [Capacities Offline Support](https://docs.capacities.io/misc/offline-support)
- [Capacities Export](https://docs.capacities.io/reference/export)
- [Capacities Storage Limits FAQ](https://docs.capacities.io/faq/account/storage-limits)
- [jem-computer/capacities-mcp (GitHub)](https://github.com/jem-computer/capacities-mcp)
- [Capacities MCP on LobeHub](https://lobehub.com/mcp/inconceivablelabs-capacitiesmcp)
