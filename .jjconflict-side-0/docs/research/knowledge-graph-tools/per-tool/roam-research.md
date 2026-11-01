# Roam Research - Phase 1 Screening

```yaml
tool_name: Roam Research
category: PKM / Knowledge Graph
official_url: https://roamresearch.com/
pricing: paid_only
pricing_details: |
  - Pro Plan: $15/month (billed annually) or $20/month (monthly billing)
  - Believer Plan: $500 for 5 years (~$8.33/month)
  - 31-day free trial available for new users
  - No free tier beyond trial
platforms:
  web_ui: yes
  desktop: yes (Mac & Windows)
  mobile: both (iOS and Android native apps)
api:
  exists: yes
  type: Datalog/Datomic (REST-style endpoints with Datalog query language)
  read_write: both
  details: |
    - Official SDK: @roam-research/roam-api-sdk (npm)
    - Endpoint: https://api.roamresearch.com/api/graph/{graph_name}/q
    - Authentication: API tokens (Bearer via X-Authorization header)
    - Query language: Datalog (similar to Datomic)
    - Operations: q (query), pull, createBlock, moveBlock, updateBlock,
      deleteBlock, createPage, updatePage, deletePage
    - Also has RoamAlphaAPI for in-app JavaScript queries
open_source: no
repo_url: https://github.com/Roam-Research (organization - tools/extensions only)
last_commit: N/A (proprietary core)
github_stars: N/A (proprietary core)
screening_result: PASS
fail_reason: null

additional_notes: |
  ## API Capabilities
  - Full CRUD operations for pages and blocks
  - Datalog query language allows powerful graph traversal and complex queries
  - Can execute raw Datomic/Datalog queries for advanced data retrieval
  - API tokens managed in Settings > Graph > API tokens

  ## MCP Server Availability
  Multiple MCP servers available for AI agent integration:

  1. **roam-research-mcp** (by 2b3pro) - Most comprehensive
     - GitHub: https://github.com/2b3pro/roam-research-mcp
     - 20+ specialized tools including:
       - roam_fetch_page_by_title, roam_search_by_text
       - roam_create_page, roam_update_page_markdown
       - roam_datomic_query (raw Datalog execution)
       - roam_remember/roam_recall (AI memory management)
       - Batch operations support
     - CLI tool included for automation
     - Supports stdio, HTTP Stream, and SSE transport
     - Docker deployment support

  2. **roam-mcp** (by PhiloSolares)
     - GitHub: https://github.com/PhiloSolares/roam-mcp
     - Simpler integration focused on Claude connectivity

  3. **Zapier MCP** - No-code integration option
     - Free up to 300 tool calls/month

  ## Data Model
  - Graph-based with bidirectional linking
  - Everything is a block (nodes in the graph)
  - Uses Datascript (ClojureScript Datalog implementation)
  - Each fact is a datom: [entity-id, attribute, value, transaction-id]
  - Supports block references, backlinks, and page references

  ## Mobile App Quality
  - iOS and Android apps exist but reviews indicate they feel like
    wrapped web views rather than native experiences
  - Issues reported: slow launch, UI jitteriness, background process killing
  - Desktop/web experience significantly better than mobile

  ## Extension Ecosystem
  - Roam Depot: Official marketplace for extensions
  - Extensions submitted via GitHub PR to roam-depot repo
  - Active developer community building tools and integrations

  ## Strengths for AI Agent Use
  - Rich query capabilities via Datalog
  - Well-maintained MCP server with comprehensive tooling
  - Batch operations for efficiency
  - Memory/recall tools designed for AI workflows
  - Multi-graph support

  ## Limitations
  - No free tier (paid only after 31-day trial)
  - Proprietary/closed source core
  - Learning curve for Datalog queries
  - Mobile experience suboptimal
  - Cloud-only (no local-first option)

additional_sections:
  mcp_integration:
    available: yes
    servers:
      - name: roam-research-mcp
        url: https://github.com/2b3pro/roam-research-mcp
        maintainer: 2b3pro
        features: [full_crud, batch_ops, memory_tools, datalog_queries]
      - name: roam-mcp
        url: https://github.com/PhiloSolares/roam-mcp
        maintainer: PhiloSolares
        features: [basic_crud]
      - name: zapier_mcp
        type: no_code
        free_tier: 300 calls/month

  query_language:
    type: Datalog
    similarity: Datomic
    power_level: high
    learning_curve: moderate_to_steep

  real_time_capabilities:
    sync: yes (cloud-based)
    collaboration: yes (multiplayer editing)
    offline: limited (desktop apps cache some data)
```

## Sources

- [Roam Research Official Website](https://roamresearch.com/)
- [Roam Research API SDK (npm)](https://www.npmjs.com/package/@roam-research/roam-api-sdk)
- [Roam Research MCP Server (2b3pro)](https://github.com/2b3pro/roam-research-mcp)
- [Roam Research MCP (PhiloSolares)](https://github.com/PhiloSolares/roam-mcp)
- [Roam Research GitHub Organization](https://github.com/Roam-Research)
- [Introduction to Roam Alpha API](https://www.putyourleftfoot.in/introduction-to-the-roam-alpha-api)
- [Datalog Queries for Roam Research](https://davidbieber.com/snippets/2020-12-22-datalog-queries-for-roam-research/)
- [Roam Mobile on App Store](https://apps.apple.com/us/app/roam-mobile/id1609277273)
- [Roam Research on Google Play](https://play.google.com/store/apps/details?id=com.roamresearch.relemma)
- [Zapier Roam Research MCP](https://zapier.com/mcp/roam-research)

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://roamresearch.com/#/app/developer-documentation (in-app) + https://www.postman.com/roamresearch
  auth_model: api_key  # Bearer token via X-Authorization header
  rate_limits: not_documented  # No official rate limit documentation found
  webhooks: no  # No native webhook support; third-party solutions exist (roam-webhook project)
  subscriptions: no  # No real-time subscription/push mechanism
  mcp_server: community  # Multiple community-maintained servers
  mcp_server_url: https://github.com/2b3pro/roam-research-mcp  # Most comprehensive option

data_model:
  node_structure: |
    - Everything is a block (nodes in the graph)
    - Blocks have a nine-character UID (:block/uid)
    - Pages are special blocks with :node/title attribute
    - Datom structure: [entity-id, attribute, value, transaction-id]
    - Uses Datascript (ClojureScript Datalog implementation)
  schema_flexibility: freeform  # No enforced schema; blocks can contain any content
  custom_fields: yes  # Via attributes in block content (e.g., [[Attribute]]:: value)
  relations: |
    - :block/refs - references to pages/blocks (creates backlinks)
    - :block/children - parent-child hierarchical relationships
    - :block/parents - all ancestor blocks (full ancestry chain)
    - :block/page - reference to containing page entity
    - Bidirectional linking via [[ ]] syntax and (( )) block references

task_support:
  native_tasks: yes  # TODO/DONE checkbox support built-in
  status_field: yes  # {{[[TODO]]}} and {{[[DONE]]}} markers
  assignee_field: workaround  # Via page references like [[Person Name]]
  due_date: workaround  # Via date references like [[January 30th, 2026]]
  dependencies: workaround  # No native dependencies; can model via tags/references
  dependency_description: |
    Dependencies not natively supported. Workarounds include:
    - Using tags like #blocked-by with block references
    - Creating pages for dependency tracking
    - Using Datalog queries to find relationships
    - Custom SmartBlocks or extensions for workflow management
  query_ready_tasks: yes  # Via Datalog queries or built-in {{query}} blocks
    # Example: Find all TODO items not tagged #blocked
    # Can query by status, date, tags, references, and custom attributes

query_capabilities:
  simple_filters: yes  # Built-in filter UI and {{query}} blocks
  graph_traversal: yes  # Full recursive traversal via Datalog ancestor rules
  multi_hop_queries: |
    Yes - Datalog supports complex multi-hop queries:
    - Find all tasks blocked by tasks owned by X (via chained :block/refs)
    - Traverse ancestry with :block/parents
    - Join across multiple entity types
    - Example: Find blocks referencing pages that reference other specific pages
  query_language: datalog  # Datascript/Datomic-style Datalog
  full_text_search: yes  # Via clojure.string/includes? in queries + UI search
  vector_search: no  # No native vector/semantic search

scale:
  documented_limits: |
    - Graph View becomes unusable around 600+ pages (layout algorithm disabled)
    - Performance degrades with large graphs (10,000+ pages)
    - Backlink-heavy pages have O(n) load times
    - Opening references can scale O(n^2) with many backlinks
    - Browser memory constraints apply (client-side processing)
  concurrent_access: yes  # Multiplayer editing supported with immutable blocks
  known_performance_issues: |
    - Initial load: 10-14 seconds regardless of graph size
    - Large graphs (10k+ pages) cause browser "Page Unresponsive" warnings
    - Client-side query execution can freeze browser for complex searches
    - Graph visualization has hard limits on page count

hosting:
  hosted_option: yes  # Cloud-only; Roam is a hosted SaaS product
  hosted_pricing: |
    - Pro: $15/month (annual) or $20/month (monthly)
    - Believer: $500 for 5 years (~$8.33/month)
  self_host_complexity: n/a  # Self-hosting not available
  self_host_requirements: n/a  # Proprietary cloud-only service
  data_export: yes  # JSON, Markdown, and EDN export formats

real_time:
  sync_mechanism: cloud_sync  # Proprietary cloud-based sync (not CRDT)
  latency: not_documented  # No public latency specifications
  conflict_resolution: |
    - Immutable blocks: users can only edit their own blocks
    - Child blocks under others' blocks are editable
    - Cloud-based conflict resolution (specifics not documented)
    - Real-time multiplayer with eventual consistency

agent_integration:
  mcp_tools_available: |
    roam-research-mcp (2b3pro) provides 20+ tools:

    Content Retrieval:
    - roam_fetch_page_by_title
    - roam_fetch_block_with_children

    Search & Discovery:
    - roam_search_by_text (full-text with namespace filtering)
    - roam_search_block_refs
    - roam_search_by_status (TODO/DONE)
    - roam_search_for_tag
    - roam_search_by_date
    - roam_find_pages_modified_today

    Content Creation:
    - roam_create_page
    - roam_update_page_markdown
    - roam_add_todo
    - roam_create_table
    - roam_create_outline
    - roam_move_block

    Advanced:
    - roam_process_batch_actions (atomic multi-operation)
    - roam_datomic_query (raw Datalog execution)
    - roam_remember / roam_recall (AI memory management)

    CLI tool: `roam` command with save, get, search, refs, update, batch, rename, status

    Transports: stdio (default), HTTP Stream (port 8088), SSE (port 8087)
    Docker deployment supported
    Multi-graph support with per-graph access control
  langchain_integration: no  # No official LangChain integration; would require custom loader
  llamaindex_integration: no  # No official LlamaIndex integration; could use JSON export
  other_integrations: |
    - Pipedream: workflow automation with Roam actions
    - Zapier MCP: no-code integration (300 free calls/month)
    - RoamJS SmartBlocks: in-app automation
    - Roam Depot: extension marketplace
    - roam-mcp (PhiloSolares): simpler MCP alternative
```

### Phase 2 Sources

- [Roam Research Official API SDK (npm)](https://www.npmjs.com/package/@roam-research/roam-api-sdk)
- [Roam Research MCP Server (2b3pro)](https://github.com/2b3pro/roam-research-mcp)
- [Roam Research Datalog Cheatsheet](https://gist.github.com/2b3pro/231e4f230ed41e3f52e8a89ebf49848b)
- [Datalog Queries for Roam Research - David Bieber](https://davidbieber.com/snippets/2020-12-22-datalog-queries-for-roam-research/)
- [Deep Dive Into Roam's Data Structure](https://www.zsolt.blog/2021/01/Roam-Data-Structure-Query.html)
- [TfT Performance: Roam Research](https://www.goedel.io/p/tft-performance-roam-research)
- [Roam Research JSON Export Format](https://davidbieber.com/snippets/2020-04-25-roam-json-export/)
- [GTD-Style Task Management in Roam](https://thesweetsetup.com/using-roam-research-for-gtd-style-task-management/)
- [Roam Research API Tracker](https://apitracker.io/a/roamresearch)
- [Roam Research Postman Collection](https://www.postman.com/roamresearch)
