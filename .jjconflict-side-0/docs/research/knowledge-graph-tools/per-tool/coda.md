# Coda - Phase 1 Screening

```yaml
tool_name: Coda
category: Task/Project Management
official_url: https://coda.io/
pricing: freemium
pricing_details: |
  Free: $0 - limited doc size (1000 rows), 50 objects per doc, 7-day version history
  Pro: $12/month ($10 annual) per Doc Maker - unlimited rows, 30-day history
  Team: $36/month ($30 annual) per Doc Maker - unlimited automations, version history
  Enterprise: Custom pricing - SAML SSO, audit logs, dedicated support
  Note: Only "Doc Makers" (who create/structure docs) are paid seats. Editors and viewers are free.
platforms:
  web_ui: yes
  desktop: no  # No official native app; community workarounds via Chrome/Edge PWA
  mobile: both  # iOS (requires iOS 15.0+) and Android (7.0+)
api:
  exists: yes
  type: REST
  read_write: both
open_source: no  # Core platform is proprietary; Packs SDK is open source
repo_url: https://github.com/coda/packs-sdk  # Packs SDK only
last_commit: 2025  # Active development on Packs SDK
github_stars: 94  # Packs SDK repo
screening_result: PASS
fail_reason:

additional_notes: |
  ## API Details
  - Base URL: https://coda.io/apis/v1
  - Authentication: Bearer token (API key)
  - Rate limits: ~200 GET requests/minute, ~10 POST requests/minute
  - Doc size limit for API: 125 MB max
  - Endpoints cover: docs, tables, rows, columns, formulas, automations, pages
  - Supports webhooks for real-time triggers (rowAdded, rowUpdated, rowRemoved)
  - Admin API available for Enterprise (https://coda.io/apis/admin/v1)

  ## MCP Server Availability
  Multiple MCP server implementations exist for AI agent integration:
  - orellazri/coda-mcp (GitHub) - official-style implementation
  - TJC-LP/coda-mcp-server (GitHub) - available via PyPI/uvx
  - Composio Coda MCP Integration
  - Zapier Coda MCP

  MCP server tools include:
  - coda_list_documents, coda_list_pages
  - coda_create_page, coda_get_page_content
  - coda_replace_page_content, coda_append_page_content

  ## Real-Time Capabilities
  - Real-time collaborative editing in web UI
  - Webhook-triggered automations for external integrations
  - Webhooks require Bearer authentication (POST only)
  - Integration platforms (Latenode, n8n, Pipedream) provide real-time sync

  ## AI Features (Native)
  - Coda AI for content generation, summarization, insights
  - AI chat for answering questions about docs
  - OpenAI Pack available for GPT-3/DALL-E integration

  ## Packs Ecosystem
  - 600+ integrations via Packs (GitHub, Slack, Jira, Google Calendar, etc.)
  - Open source Packs SDK (TypeScript) for building custom integrations
  - Packs can be built using GitHub Codespaces

  ## Agent Orchestration Relevance
  - Strong API for programmatic doc/table/row manipulation
  - Good MCP ecosystem for Claude/AI agent integration
  - Webhook support enables event-driven automation
  - Packs SDK allows custom integration development
  - Free tier limitations (1000 rows, 50 objects) may constrain agent workflows
  - Rate limits (especially 10 POST/min) could bottleneck heavy write operations

additional_sections:
  collaboration_model: |
    - Real-time collaborative editing
    - Doc Maker / Editor / Viewer permission levels
    - Workspace-based organization
    - Private folders on Team+ plans
  automation_capabilities: |
    - Native automations with triggers and actions
    - Time-based and event-based triggers
    - Webhook-triggered automations
    - Unlimited automations on Team+ plans (limited on lower tiers)
  data_model: |
    - Docs contain pages, tables, views, buttons, formulas
    - Tables with typed columns (text, number, date, relations, etc.)
    - Cross-doc tables for data sharing between docs
    - Views: tables, boards, calendars, Gantt charts
```

## Sources

- [Coda Official Website](https://coda.io/)
- [Coda API Reference Documentation](https://coda.io/developers)
- [Coda Pricing](https://coda.io/pricing)
- [Coda Packs SDK GitHub](https://github.com/coda/packs-sdk)
- [Coda Mobile App Basics](https://help.coda.io/hc/en-us/articles/39555863271053-Coda-mobile-app-basics)
- [Coda Webhook Automations](https://help.coda.io/en/articles/6170802-create-webhook-triggered-automations)
- [orellazri/coda-mcp GitHub](https://github.com/orellazri/coda-mcp)
- [TJC-LP/coda-mcp-server GitHub](https://github.com/TJC-LP/coda-mcp-server)
- [Coda MCP on Composio](https://mcp.composio.dev/coda)
- [Coda API Rate Limits Discussion](https://community.coda.io/t/request-rate-limit-and-the-maximum-number-of-rows/8231)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://coda.io/developers
  auth_model: api_key  # Bearer token authentication
  rate_limits: |
    - Reading data: 100 requests per 6 seconds
    - Writing data (POST/PUT/PATCH): 10 requests per 6 seconds
    - Writing doc content: 5 requests per 10 seconds
    - Listing docs: 4 requests per 6 seconds
    - Reading analytics: 100 requests per 6 seconds
    - HTTP 429 returned when exceeded; exponential backoff recommended
  webhooks: yes  # rowAdded, rowUpdated, rowRemoved events
  subscriptions: no  # Webhooks are push-based, no persistent subscriptions
  mcp_server: community  # Multiple community implementations
  mcp_server_url: |
    - https://github.com/orellazri/coda-mcp (10 tools, page-focused)
    - https://github.com/TJC-LP/coda-mcp-server (26 tools, comprehensive)
    - https://github.com/universal-mcp/coda (Universal MCP framework)

data_model:
  node_structure: |
    Hierarchical document model:
    - Workspace > Docs > Pages/Tables/Views/Formulas
    - Tables contain rows with typed columns
    - Pages contain rich text, embedded tables, and interactive controls
    - Views are filtered/sorted representations of tables (board, calendar, timeline, Gantt)
  schema_flexibility: dynamic  # Columns can be added/modified; types are enforced per column
  custom_fields: yes  # Any column type can be added to tables
  relations: |
    - Relation columns link rows across tables (supports bi-directional)
    - Lookup columns pull data from related rows
    - Cross-doc sync tables share data between documents
    - Many-to-many relationships supported via lookup formulas
    - Multidimensional schemas where tables connect to each other

task_support:
  native_tasks: no  # No built-in task type; tasks are rows in tables
  status_field: yes  # Can create status columns (select/dropdown type)
  assignee_field: yes  # Can create people columns for assignees
  due_date: yes  # Date columns supported
  dependencies: workaround
  dependency_description: |
    Dependencies implemented via relation columns:
    - "Blocked by" column: manual selection of parent tasks
    - "Blocking" column: formula-based reciprocal lookup
    - Enforced dependencies on timeline view auto-update dates when predecessor moves
    - Multiple dependencies supported via "Allow multiple selections" option
    - Circular dependency prevention built into UI
    - Visual arrows on timeline/Gantt views show dependencies
  query_ready_tasks: yes  # Filter formula can query "tasks where [Blocked By].Count() = 0 AND Status != Complete"

query_capabilities:
  simple_filters: yes  # Extensive filter builder and Filter() formula
  graph_traversal: no  # No native graph traversal; limited to one-hop lookups
  multi_hop_queries: |
    Limited support via chained lookup formulas:
    - Can do: "tasks where BlockedBy.Owner contains X"
    - Cannot do: arbitrary depth traversal
    - Requires pre-defining the relationship path in formulas
    - No recursive queries or transitive closure
  query_language: other  # Coda formula language (proprietary, Excel-like)
  full_text_search: yes  # Basic search within docs and tables
  vector_search: no  # No native vector/semantic search

scale:
  documented_limits: |
    Free plan (shared docs):
    - 1000 rows per doc
    - 50 objects (pages, tables, views, buttons, formulas) per doc
    - 100 rows per Pack sync table

    Pro/Team/Enterprise:
    - No row limits
    - 10,000 rows per Pack sync table

    All plans:
    - 125 MB doc size limit for API access
    - 1 GB media storage per account
    - 10 MB per file attachment
    - Formula calculation limits (warnings at threshold)
  concurrent_access: |
    - Real-time collaborative editing supported
    - Multiple users can edit simultaneously
    - Over 50,000 teams use Coda
    - No documented concurrent user limits per doc
  known_performance_issues: |
    - Large docs (approaching formula calculation limits) may disable calculations
    - Docs exceeding 125 MB cannot use API, Cross-doc, or Zapier
    - Performance degrades with complex formulas on large tables
    - Recommended to split large docs into focused, smaller docs

hosting:
  hosted_option: yes  # Cloud-only SaaS
  hosted_pricing: |
    - Free: $0
    - Pro: $10-12/month per Doc Maker
    - Team: $30-36/month per Doc Maker
    - Enterprise: Custom pricing
  self_host_complexity: n/a  # No self-hosting option
  self_host_requirements: n/a
  data_export: yes  # CSV, JSON (via API), Markdown/HTML (via third-party tools)

real_time:
  sync_mechanism: |
    - Proprietary real-time sync for collaborative editing
    - Webhooks for external integrations (POST-based, requires auth)
    - No public WebSocket API
    - Polling required for API-based real-time updates
  latency: not documented  # Real-time in UI; API subject to rate limits
  conflict_resolution: |
    Not documented publicly. Platform supports simultaneous editing
    with version history for recovery. Likely uses operational transformation
    or similar approach (implementation not disclosed).

agent_integration:
  mcp_tools_available: |
    orellazri/coda-mcp (10 tools):
    - coda_list_documents, coda_list_pages
    - coda_create_page, coda_get_page_content
    - coda_replace_page_content, coda_append_page_content
    - coda_duplicate_page, coda_rename_page
    - coda_peek_page, coda_resolve_link

    TJC-LP/coda-mcp-server (26 tools):
    - Document: list_docs, get_doc_info, create_doc, update_doc, delete_doc
    - Pages: list_pages, get_page, create_page, update_page, delete_page, export workflow
    - Tables: list_tables, get_table, list_columns, get_column
    - Rows: list_rows, get_row, upsert_rows, update_row, delete_row, delete_rows, push_button
    - Formulas: list_formulas, get_formula
    - Auth: whoami
  langchain_integration: no  # No official LangChain integration found
  llamaindex_integration: no  # No official LlamaIndex integration found
  other_integrations: |
    - 600+ Packs ecosystem (GitHub, Slack, Jira, Google, etc.)
    - Zapier integration
    - n8n, Pipedream, Latenode workflow platforms
    - Native Coda AI features (content generation, summarization)
    - OpenAI Pack for GPT/DALL-E integration
    - Packs SDK (TypeScript) for custom integrations
```

## Phase 2 Sources

- [Coda API Reference Documentation](https://coda.io/developers)
- [Coda API Rate Limits](https://community.coda.io/t/api-rate-limit-for-different-funcitons/43993)
- [Overview: Doc Limits](https://help.coda.io/hc/en-us/articles/39555760015757-Overview-Doc-limits)
- [Create Dependencies in Your Project Tracker](https://help.coda.io/hc/en-us/articles/39555816244877-Create-dependencies-in-your-project-tracker)
- [Task Tracking with Dependencies Template](https://coda.io/@hales/task-tracking-with-dependencies)
- [Use the Filter Formula](https://help.coda.io/hc/en-us/articles/39555813909261-Use-the-Filter-formula)
- [Set Up Cross-doc Sync Tables](https://help.coda.io/hc/en-us/articles/39555763704461-Set-up-Cross-doc-sync-tables)
- [Designing Your Doc with Schemas](https://coda.io/@coda/designing-your-doc-with-schemas/multidimensional-data-without-hierarchy-8)
- [Scaling Up Your Coda Docs](https://coda.io/resources/guides/too-many-rows)
- [Overview: Export Data from Coda](https://help.coda.io/en/articles/1222787-overview-export-data-from-coda)
- [Create Webhook-Triggered Automations](https://help.coda.io/hc/en-us/articles/39555972006541-Create-webhook-triggered-automations)
- [orellazri/coda-mcp GitHub](https://github.com/orellazri/coda-mcp)
- [TJC-LP/coda-mcp-server GitHub](https://github.com/TJC-LP/coda-mcp-server)
- [Coda MCP on Composio](https://mcp.composio.dev/coda)
