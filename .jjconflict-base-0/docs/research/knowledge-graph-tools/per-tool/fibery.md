# Fibery - Phase 1 Screening

```yaml
tool_name: Fibery
category: Task/Project Management
official_url: https://fibery.com/
pricing: freemium
pricing_details: |
  Free: $0 - up to 10 users + 10 guests, 10 databases, unlimited entities, 14-day version history, limited AI
  Standard: $12/user/month (annual) - unlimited databases, charts, whiteboards, human support, enhanced AI
  Pro: $20/user/month (annual) - user groups, advanced permissions, JavaScript automations, unlimited version history
  Enterprise: $40/user/month (annual, min 25 users) - unlimited automations/integrations, SAML SSO, SCIM, regional data residency
  Special: 50% off for non-profits/education, 100% off for open-source, 6 months free Pro for startups
platforms:
  web_ui: yes
  desktop: Windows, macOS
  mobile: both  # iOS and Android apps launched December 2025
api:
  exists: yes
  type: GraphQL  # Primary API; also has REST-like commands API and Webhooks
  read_write: both
open_source: no  # Core product is proprietary SaaS
repo_url: https://github.com/Fibery-inc  # Organization page with some open source tools
last_commit: N/A  # Core product not open source
github_stars: N/A  # Core product not open source
screening_result: PASS
fail_reason:

additional_notes: |
  ## Key Strengths for AI Agent Orchestration

  ### Official MCP Server
  Fibery provides an official Model Context Protocol (MCP) server at https://github.com/Fibery-inc/fibery-mcp-server
  - 27 stars, 17 forks, MIT licensed
  - Enables Claude and other MCP-compatible AI tools to interact with Fibery workspaces
  - Capabilities: query databases, create/update entities, search Fibery Guide documentation
  - Tools include: list_databases, describe_database, query_database, create_entity, create_entities_batch, update_entity
  - Python-based (3.10+), can be installed via Smithery CLI or manual UV setup

  ### GraphQL API
  - Full read/write capabilities via GraphQL
  - Per-space endpoints: https://YOUR_ACCOUNT.fibery.io/api/graphql/space/YOUR_SPACE
  - Built-in GraphQL IDE for exploration and testing
  - Token-based authentication via Authorization header
  - Comprehensive documentation at https://the.fibery.io/@public/User_Guide/Guide/GraphQL-API-254

  ### Webhook Support
  - Webhooks for external system integration
  - Can trigger on entity creation and updates
  - Enables real-time sync with external systems

  ### Built-in AI Features
  - AI Workspace Assistant for creating spaces, writing formulas, searching data
  - AI in Automations - run AI tasks via automation rules or buttons
  - AI Search for reference creation (linking feedback to features/bugs)
  - 2x faster AI agent as of November 2025
  - AI can create automation rules or buttons (August 2025 beta)

  ### Automation Capabilities
  - No-code automation rules and buttons
  - JavaScript automations on Pro plan
  - Unlimited automations on Enterprise plan
  - Third-party integration via n8n, Latenode, Pipedream

  ### Data Model
  - Flexible no-code platform with custom databases and relations
  - Multiple views: Board, Table, List, Timeline, Calendar, Feed, Chart, Map, Form, Entity
  - Rich text documents with embedded entities
  - Whiteboards for diagramming and mind maps

  ### Integrations
  - Native: Slack, GitHub, Intercom, Stripe (Dec 2025), n8n
  - Supports importing from Notion, Jira, and other tools
  - Web extension available (open source on GitHub)

  ## Potential Limitations
  - Not a pure knowledge graph tool - more of a flexible work management platform
  - GraphQL API rather than native graph query language
  - Some advanced features require Pro/Enterprise tiers
  - Regional data residency only on Enterprise plan

additional_sections:
  mcp_server:
    available: yes
    official: yes
    repo: https://github.com/Fibery-inc/fibery-mcp-server
    stars: 27
    license: MIT
  real_time_capabilities:
    webhooks: yes
    streaming_api: no
    push_notifications: yes  # Mobile app supports push notifications
  ai_native_features:
    built_in_ai: yes
    ai_automations: yes
    ai_search: yes
    mcp_integration: yes
  company_info:
    founded: ~2018
    business_model: SaaS subscription
    profitability: Near profitable as of 2025 (85% MRR growth)
    headquarters: Unknown
```

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://api.fibery.io/ (redirects to https://the.fibery.io/@public/User_Guide/Guide/Fibery-API-Overview-279)
  auth_model: api_key  # Token-based auth via "Authorization: Token ${api_key}" header
  rate_limits: 3 requests per second per token (HTTP 429 on exceed)
  webhooks: yes  # Add, delete, get webhooks; triggers on entity create/update
  subscriptions: no  # GraphQL subscriptions not documented; use webhooks or polling
  mcp_server: official
  mcp_server_url: https://github.com/Fibery-inc/fibery-mcp-server

data_model:
  node_structure: |
    Fibery uses a flexible "Type" (database) system where:
    - Types are custom-defined templates for entities (Bugs, Teams, Tasks, etc.)
    - Each Type has custom fields: number, rich text, single/multi-select, date, workflow, assignment, formulas
    - Entities are instances of Types with field values
    - Spaces organize related Types/databases
  schema_flexibility: dynamic  # Users can create/modify Types and fields at any time
  custom_fields: yes  # Extensive custom field support including formulas
  relations: |
    - One-to-many relations (e.g., Project has many Tasks)
    - Many-to-many relations
    - Self-relations (e.g., Task depends on Task)
    - Special "Dependency" relation type for blocking/blocked-by relationships
    - Relations are first-class citizens, queryable via GraphQL

task_support:
  native_tasks: yes  # Tasks are a common use case; can be any custom Type
  status_field: yes  # "Workflow" field type provides customizable statuses
  assignee_field: yes  # "Assignment" field type for user assignment
  due_date: yes  # Date fields with optional time
  dependencies: yes  # Native "Dependency" relation type
  dependency_description: |
    Fibery provides a dedicated Dependency relation type that enables:
    - Blocking/blocked-by relationships between any entities
    - Visual dependencies on Timeline/Gantt views
    - Automatic date adjustment when moving dependent items
    - Bottleneck identification and tracking
  query_ready_tasks: workaround  # |
    # No direct "tasks with no blockers" query documented, but can be achieved by:
    # - Filtering on the "blocked by" relation being empty
    # - Using GraphQL inner list filtering with isEmpty operator
    # - Creating Smart Folders with appropriate filters

query_capabilities:
  simple_filters: yes  # Extensive filter operators
  graph_traversal: yes  # GraphQL nested queries traverse relations
  multi_hop_queries: |
    GraphQL supports nested queries across relations. For example:
    - Query Tasks -> Project -> Owner
    - Query Features -> Bugs -> Assignee
    The Fibery API (non-GraphQL) also supports structured queries with q/and, q/or operators
    Multi-hop "find all tasks blocked by tasks owned by X" is achievable via nested filtering
  query_language: graphql  # Primary; also has REST-like Commands API with custom query syntax
  full_text_search: yes  # Built-in full-text search across entities
  vector_search: yes  # |
    # AI Search uses semantic/vector search combined with keyword search
    # Embeddings technology for clustering and relevance ranking
    # Dual-search model: vector DB + keyword DB merged results

scale:
  documented_limits: |
    - No hard database/entity limits (customers have 100k+ records in single database)
    - No theoretical database count limit
    - Free plan: 10 databases; paid plans: unlimited
    - Export of 100k entities may take up to 1 hour
    - Performance note: >500k data points may benefit from aggregation
    - Tables handle large volumes better than Boards
    - Complex formulas slower in large databases
  concurrent_access: |
    - Multi-user real-time collaboration supported
    - Live updates and sync across users
    - User groups for permission management
    - No documented hard concurrent user limits
  known_performance_issues: |
    - Performance depends on usage patterns
    - Complex formulas in large databases can be slow
    - Limited offline functionality
    - Mobile app has fewer features than desktop

hosting:
  hosted_option: yes  # SaaS-only
  hosted_pricing: |
    Free: $0 (10 users, 10 DBs)
    Standard: $12/user/month
    Pro: $20/user/month
    Enterprise: $40/user/month (min 25 users)
  self_host_complexity: n/a  # No self-hosted option available
  self_host_requirements: n/a  # Hosted on AWS; no on-premise option
  data_export: yes  # |
    # Full data export available
    # Exports all entities as archive (email notification with download link)
    # Can take up to 1 hour for 100k+ entities

real_time:
  sync_mechanism: |
    - WebSocket connections for real-time UI updates (internal)
    - Webhooks for external integrations (push on entity changes)
    - Polling required for API-based real-time needs
    - No public GraphQL subscriptions documented
  latency: not documented  # Real-time for UI; webhook latency not specified
  conflict_resolution: |
    - Not explicitly documented
    - Platform handles concurrent edits in collaborative editing
    - Rich text editor supports real-time collaboration
    - No CRDT or OT documentation publicly available

agent_integration:
  mcp_tools_available: |
    - list_databases: Retrieve all accessible databases in workspace
    - describe_database: Get database structure (fields, types)
    - query_database: Flexible data queries via Fibery API
    - create_entity: Add new entities with field values
    - create_entities_batch: Bulk entity creation
    - update_entity: Modify existing entity fields
  langchain_integration: no  # No native integration; possible via MCP adapters
  llamaindex_integration: no  # No native integration; possible via MCP adapters
  other_integrations: |
    - n8n: Native integration for workflow automation
    - Pipedream: Fibery API connector available
    - Latenode: Supported integration platform
    - MCP: Via langchain-mcp-adapters, any MCP server can be used with LangChain/LlamaIndex
    - Built-in AI: Workspace assistant, AI automations, AI search
```

### Phase 2 Research Notes

#### API Capabilities Summary
- **GraphQL API**: Per-space endpoints at `https://ACCOUNT.fibery.io/api/graphql/space/SPACE`
- **Commands API**: REST-like at `https://ACCOUNT.fibery.io/api/commands` with operations like `fibery.entity/create`, `fibery.entity/query`, `fibery.schema/batch`
- **Webhooks API**: Add, delete, list webhooks for entity change notifications
- **Rate limit**: 3 req/sec/token; returns HTTP 429 when exceeded

#### GraphQL Query Features
- Filter operators: `is`, `isNot`, `contains`, `notContains`, `greater`, `greaterOrEquals`, `less`, `lessOrEquals`, `in`, `notIn`, `isNull`
- Inner list filtering: `isEmpty`, `contains` (AND), `containsAny` (OR), `notContains` (AND), `notContainsAny` (OR)
- Nested queries for relation traversal
- Pagination support

#### Key Strengths for Agent Use
1. **Official MCP Server** - Direct integration with Claude and MCP-compatible tools
2. **Flexible Data Model** - Can model any domain with custom types and relations
3. **Native Dependencies** - Built-in blocking/blocked-by relationship type
4. **AI Features** - Built-in AI search with semantic/vector capabilities

#### Limitations
1. **No GraphQL Subscriptions** - Real-time requires webhooks or polling
2. **No Self-Hosting** - SaaS only (AWS hosted)
3. **No Native LangChain/LlamaIndex** - Must use MCP adapters
4. **Rate Limits** - 3 req/sec may be limiting for high-throughput agents
