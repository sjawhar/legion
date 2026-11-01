# Linear - Phase 1 Screening

```yaml
tool_name: Linear
category: Task/Project Management
official_url: https://linear.app
pricing: freemium
pricing_details: |
  Free plan: $0/user - up to 250 active issues, unlimited archived, 2 teams max, API/webhooks included
  Basic: $8/user/month ($6.40 annual) - unlimited issues, admin roles, unlimited file uploads
  Business: $14/user/month - private teams, guest accounts, advanced analytics
  Enterprise: Custom pricing - SSO, SCIM, advanced API rate limits, custom security reviews
  Discounts: 20% off annual billing, startup program (6 months free), education discounts
platforms:
  web_ui: yes
  desktop: yes  # macOS, Windows
  mobile: both  # iOS and Android native apps
api:
  exists: yes
  type: GraphQL
  read_write: both
  endpoint: https://api.linear.app/graphql
  authentication: API keys (personal) or OAuth 2.0 (apps)
  rate_limits: 1,500 requests/hour per user (standard), higher for Enterprise
  sdk: Official TypeScript SDK available (@linear/sdk)
open_source: no  # Core product is proprietary
repo_url: https://github.com/linear/linear
last_commit: 2026-01-27  # Based on @linear/sdk@72.0.0 release
github_stars: 1200  # SDK/tools monorepo, not the main product
screening_result: PASS
fail_reason:

additional_notes: |
  ## Key Strengths for AI Agent Orchestration

  ### Official MCP Server
  Linear has an official Model Context Protocol (MCP) server, making it a first-class citizen
  for AI agent integration. Configuration is straightforward:
  - URL: https://mcp.linear.app/mcp
  - Supports OAuth tokens and API keys via Authorization header
  - Works natively with Claude, Cursor, and other MCP-compatible clients

  ### GraphQL API Advantages
  - Full read/write access to all entities (issues, projects, teams, cycles, etc.)
  - Strongly typed schema with TypeScript SDK
  - Same API used internally by Linear's own apps
  - Supports complex queries in single requests (GraphQL efficiency)

  ### Real-Time Capabilities
  - Webhooks for data change notifications (issues, comments, projects, cycles, etc.)
  - Supports Issue, Comment, Project, Cycle, Label, User, and SLA change events
  - Retry mechanism with backoff (1 min, 1 hour, 6 hours)
  - HMAC-SHA256 signature verification for security
  - All API mutations observed in real-time by clients

  ### Native AI Features
  - Semantic search across titles, descriptions, feedback, and support tickets
  - AI-generated project/initiative summaries (daily/weekly digest)
  - Automatic deadline application for time-sensitive tasks
  - Integrations with AI coding tools (Cursor, Claude, ChatGPT)

  ### Developer Experience
  - Keyboard-driven interface (designed for developers)
  - Deep GitHub integration with automatic PR/issue linking
  - 100+ integrations (Slack, Figma, Sentry, etc.)
  - API access available on free tier

  ## Limitations
  - Free tier limited to 250 active issues (unlimited archived)
  - Free tier limited to 2 teams
  - No REST API (GraphQL only - may require learning curve)
  - Mobile app stability noted as occasional concern in reviews

  ## Agent Use Cases
  - Automated issue triage and categorization
  - Sprint planning assistance
  - Progress reporting and summaries
  - Cross-tool workflow automation (GitHub <-> Linear)
  - Customer feedback to issue pipeline

additional_sections:
  mcp_support:
    official_server: yes
    server_url: https://mcp.linear.app/mcp
    auth_methods: [OAuth, API_key]
    compatible_clients: [Claude, Cursor, "other MCP clients"]

  ai_native_features:
    semantic_search: yes
    ai_summaries: yes
    auto_deadlines: yes
    ai_tool_integrations: [Cursor, Claude, ChatGPT]

  real_time:
    webhooks: yes
    websocket: no  # Not documented
    polling_alternative: yes  # Via GraphQL queries

  integrations_count: "100+"
  key_integrations:
    - GitHub
    - GitLab
    - Slack
    - Figma
    - Sentry
    - Zendesk
    - Intercom
```

## Sources

- [Linear Official Website](https://linear.app)
- [Linear Pricing](https://linear.app/pricing)
- [Linear API Documentation](https://developers.linear.app)
- [Linear GraphQL Getting Started](https://linear.app/developers/graphql)
- [Linear MCP Server Documentation](https://linear.app/docs/mcp)
- [Linear Webhooks Documentation](https://linear.app/developers/webhooks)
- [Linear AI Features](https://linear.app/ai)
- [Linear GitHub Repository](https://github.com/linear/linear)
- [Linear Mobile Apps](https://linear.app/mobile)
- [Linear Download Page](https://linear.app/download)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://developers.linear.app
  auth_model: api_key and oauth  # Personal API keys or OAuth 2.0 for apps
  rate_limits:
    requests_per_hour:
      api_key: 5000  # Per user, shared across all keys
      oauth_app: 500  # Per user/app combination
      unauthenticated: 60  # Per IP address
    complexity_points_per_hour:
      api_key: 250000
      unauthenticated: 10000
    max_query_complexity: 10000  # Single query limit
    algorithm: leaky_bucket  # Constant refill rate
    enterprise: higher limits negotiable
  webhooks: yes  # Issues, Comments, Projects, Cycles, Labels, Users, SLAs
  subscriptions: no  # No GraphQL subscriptions, use webhooks instead
  mcp_server: official
  mcp_server_url: https://mcp.linear.app/mcp  # Also available at /sse for SSE transport

data_model:
  node_structure: |
    Hierarchical object graph with normalized entities:
    - Workspaces contain Teams, Users, Projects, Initiatives
    - Teams contain Issues, Cycles, Labels, Workflow States
    - Issues have parent/child (sub-issues), relations (blocks/blocked by/related/duplicate)
    - Projects group Issues across teams, have Milestones
    - Cycles are time-boxed sprints within teams
  schema_flexibility: fixed  # Predefined entity types with optional custom fields
  custom_fields: yes  # Available on Standard plan ($8/user/month) and above
  relations: |
    - Issue relations: blocks, blocked_by, related, duplicate
    - Parent/child (sub-issues)
    - Issue -> Team, Project, Cycle, Assignee, Creator, Labels
    - Project -> Team(s), Issues, Milestones, Initiatives
    - API note: Only returns forward "blocks" direction, not reverse "blocked_by"

task_support:
  native_tasks: yes  # Issues are the core task primitive
  status_field: yes  # WorkflowState with types: triage, backlog, unstarted, started, completed, canceled
  assignee_field: yes  # Single assignee per issue
  due_date: yes  # Due date field on issues
  dependencies: yes  # Native blocking/blocked by relations
  dependency_description: |
    Issues support "blocks" and "blocked by" relations via the relations system.
    Visual indicators: orange flag for "blocked by", red flag for "blocks".
    When blocking issue resolves, flag turns green and moves to "related".
    Keyboard shortcuts: M+B (blocked by), M+X (blocks), M+R (relate).
    Project-level dependencies also supported with timeline visualization.
  query_ready_tasks: workaround  # See multi_hop_queries below

query_capabilities:
  simple_filters: yes  # Filter by status, priority, assignee, labels, dates, etc.
  graph_traversal: partial  # Can filter on relations, but limited to one hop
  multi_hop_queries: |
    Limited. API only returns forward "blocks" dependencies, not reverse "blocked_by".
    To find unblocked tasks, you would need to:
    1. Query all issues
    2. Query all IssueRelations of type "blocks"
    3. Filter client-side to find issues not in any "blocks" target
    Cannot do "tasks blocked by tasks owned by X" in single query.
  query_language: graphql  # Full GraphQL with introspection
  full_text_search: yes  # Semantic search across titles, descriptions, feedback
  vector_search: yes  # Built-in semantic/AI-powered search

scale:
  documented_limits:
    free_tier: 250 active issues, 2 teams, 10MB file uploads
    paid_tiers: unlimited issues, teams, file uploads
    api_complexity: 10000 points max per query
  concurrent_access: yes  # Multi-user with real-time sync
  known_performance_issues: |
    - Mobile app stability occasionally mentioned in reviews
    - Large workspaces may hit complexity limits on deep queries
    - Polling discouraged; use webhooks for updates

hosting:
  hosted_option: yes  # SaaS only
  hosted_pricing: |
    Free: $0/user (250 issues, 2 teams)
    Standard: $8/user/month ($6.40 annual)
    Business: $14/user/month
    Enterprise: Custom pricing
  self_host_complexity: n/a  # No self-hosted option
  self_host_requirements: n/a
  data_export: yes  # CSV export from admin settings, API export, webhook streaming

real_time:
  sync_mechanism: |
    Custom sync engine (not CRDT-based for most entities).
    - Object graph synced between client and server
    - Total ordering via incremental sync_id (version number)
    - MobX-based in-memory layer on frontend
    - Offline support with local persistence
    - CRDT used only for issue descriptions (recently added)
  latency: |
    Near-instant for connected clients.
    Webhooks retry: 1 min, 1 hour, 6 hours.
  conflict_resolution: |
    Server-authoritative with total ordering (similar to OT, not CRDT).
    LastSyncId serves as database version number.
    Transactions follow strict order; client optimistic updates reconcile on sync.
    Partial sync: only open issues + recently closed sent to clients.

agent_integration:
  mcp_tools_available:
    - linear_create_issue  # Create issues with title, team, description, priority, status
    - linear_update_issue  # Update existing issues
    - linear_search_issues  # Search with text and filters (assignee, status, priority, dates, labels)
    - linear_get_issue  # View issue details with relationships
    - linear_create_comment  # Add comments to issues
    - linear_list_projects  # List projects
    - linear_create_project_update  # Create project updates with health status
    # More functionality planned per Linear docs
  langchain_integration: no  # No official integration found
  llamaindex_integration: no  # No official integration found
  other_integrations:
    - Official TypeScript SDK (@linear/sdk)
    - Composio MCP integration
    - Community MCP servers (jerhadf, mkusaka, cosmix, etc.)
    - Airbyte connector for data sync
    - Google Sheets integration
    - 100+ native integrations (GitHub, Slack, Figma, Sentry, etc.)
```

## Phase 2 Sources

- [Linear Rate Limiting Documentation](https://developers.linear.app/docs/graphql/working-with-the-graphql-api/rate-limiting)
- [Linear Filtering Documentation](https://linear.app/developers/filtering)
- [Linear Issue Relations Documentation](https://linear.app/docs/issue-relations)
- [Linear Project Dependencies](https://linear.app/docs/project-dependencies)
- [Linear MCP Server Documentation](https://linear.app/docs/mcp)
- [Linear Webhooks Documentation](https://linear.app/developers/webhooks)
- [Linear GraphQL Schema on GitHub](https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql)
- [Linear API on Apollo Studio](https://studio.apollographql.com/public/Linear-API/schema/reference)
- [Linear Data Export Documentation](https://linear.app/docs/exporting-data)
- [Linear Import Documentation](https://linear.app/docs/import-issues)
- [Linear Custom Views](https://linear.app/docs/custom-views)
- [Linear Sync Engine Analysis](https://www.fujimon.com/blog/linear-sync-engine)
- [Reverse Engineering Linear's Sync Engine](https://github.com/wzhudev/reverse-linear-sync-engine)
- [Linear MCP Server on Glama](https://glama.ai/mcp/servers/@cosmix/linear-mcp)
