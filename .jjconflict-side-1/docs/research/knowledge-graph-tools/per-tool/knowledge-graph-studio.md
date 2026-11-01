# Knowledge Graph Studio (WhyHow)

```yaml
tool_name: Knowledge Graph Studio
category: AI-Agent Native
official_url: https://whyhow.ai/
pricing: open_source
pricing_details: |
  Open source under MIT license (self-hosted). Enterprise cloud-hosted version
  with advanced UI available via contact/licensing. No published pricing tiers;
  enterprise pricing requires contacting team@whyhow.ai.
platforms:
  web_ui: yes
  desktop: no
  mobile: no
api:
  exists: yes
  type: REST
  read_write: both
open_source: yes
repo_url: https://github.com/whyhow-ai/knowledge-graph-studio
last_commit: 2024-12-25
github_stars: 894
screening_result: PASS
fail_reason: null

additional_notes: |
  ## Overview
  WhyHow Knowledge Graph Studio is a platform specifically designed for creating
  and managing RAG-native knowledge graphs. It takes an opinionated approach to
  knowledge graph construction optimized for LLM/agent workflows.

  ## Key Differentiators for AI Agent Orchestration

  ### Small Graph Philosophy
  WhyHow's core design philosophy centers on creating many small, scoped graphs
  rather than one large graph. Each small graph is a self-contained unit that
  can be called upon agentically, with its own logic, rule-sets, and data pipelines.
  This is explicitly designed for multi-agent RAG systems.

  ### Vector Chunks as First-Class Citizens
  The platform treats vector chunks as core primitives in graphs, making depth,
  context, and explainability native to data retrieval. This supports context-aware
  information retrieval that enriches RAG workflows.

  ### No Text2Cypher Dependency
  WhyHow's query engine does not utilize Text2Cypher, claiming up to 2x more
  accurate results in benchmarks compared to Text2Cypher approaches.

  ## Technical Details
  - Built on MongoDB (NoSQL) for flexible schema support
  - FastAPI backend with Swagger UI documentation
  - Python SDK available via PyPI (`pip install whyhow`)
  - OpenAI API integration for embeddings and generation
  - Docker support included
  - LangChain integration demonstrated (LLMGraphTransformer)

  ## API Capabilities
  - Workspace management
  - Document and chunk operations
  - Triple creation and management
  - Graph construction and querying
  - Rule-based entity resolution
  - Schema management

  ## Enterprise Features (Cloud-Hosted)
  - Multiplayer/collaborative graph creation
  - Human-in-the-loop workflows
  - Advanced visualization UI
  - Agentic infrastructure design services

  ## Limitations Observed
  - No MCP server implementation found
  - No mobile apps
  - Desktop app not available (web-only)
  - Enterprise pricing not publicly disclosed
  - Last commit was December 2024 (moderate activity)

additional_sections:
  agent_integration:
    langchain: yes
    llamaindex: partial # Demonstrated in docs but not native integration
    mcp_server: no
    custom_sdk: yes # Python SDK available

  rag_features:
    vector_storage: yes
    chunk_management: yes
    entity_resolution: yes
    schema_enforcement: yes
    graph_querying: yes

  collaboration:
    multi_user: yes # Enterprise only
    human_in_loop: yes # Enterprise only
    graph_sharing: yes

  deployment_options:
    self_hosted: yes
    cloud_hosted: yes # Enterprise
    on_premise: yes # Via enterprise licensing
```

## Sources
- [GitHub Repository](https://github.com/whyhow-ai/knowledge-graph-studio)
- [Official Website](https://whyhow.ai/)
- [SDK Documentation](https://whyhow-ai.github.io/whyhow-sdk-docs/)
- [Quick Start Guide](https://docs.whyhow.ai/docs/getting-started/quick-start/)
- [WhyHow KG Studio Platform Beta Announcement](https://medium.com/enterprise-rag/whyhow-ai-kg-studio-platform-beta-rag-native-graphs-1105e5a84ff2)
- [Open-Sourcing Announcement](https://medium.com/enterprise-rag/open-sourcing-the-whyhow-knowledge-graph-studio-powered-by-nosql-edce283fb341)
- [Choosing WhyHow Knowledge Graph Studio](https://medium.com/enterprise-rag/choosing-the-whyhow-ai-knowledge-graph-studio-8ed38f1820c3)

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://whyhow-ai.github.io/whyhow-sdk-docs/api/
  auth_model: api_key  # Generated via CLI script, stored per user
  rate_limits: self-managed  # No documented rate limits; self-hosted means you control limits
  webhooks: no
  subscriptions: no  # No real-time push updates documented
  mcp_server: none  # No MCP server implementation found
  mcp_server_url: null

data_model:
  node_structure: |
    Nodes represent entities with name and label (entity type). Nodes can have
    arbitrary properties and are linked to chunks (source text segments) for
    provenance. Triples connect nodes via head-relation-tail structures. The
    platform uses 11 MongoDB collections: chunk, document, graph, node, query,
    rule, schema, task, triple, user, and workspace.
  schema_flexibility: dynamic  # Supports exploratory (freeform) and schema-constrained graphs
  custom_fields: yes  # Nodes support arbitrary properties
  relations: |
    Relations are defined as triples (head node -> relation -> tail node). Triples
    store embedded vectors for semantic similarity search. Relations can be typed
    via schema definitions specifying allowed_nodes and allowed_relationships.
    Chunks are linked to triples for evidence/provenance tracking.

task_support:
  native_tasks: partial  # Has a "task" collection for async operation tracking, not project tasks
  status_field: yes  # Tasks have status for tracking long-running operations
  assignee_field: no  # No user assignment for tasks
  due_date: no
  dependencies: workaround  # Can model dependencies as graph relationships, not native task deps
  dependency_description: |
    The "task" collection is for tracking async API operations (like document uploads),
    not project/work tasks. Task dependencies for work items would need to be modeled
    as graph relationships (e.g., "Task A" -[BLOCKS]-> "Task B") but this is not a
    native feature - you build it yourself using the graph primitives.
  query_ready_tasks: no  # No built-in concept of "unblocked tasks"

query_capabilities:
  simple_filters: yes  # Filter by workspace, document, graph, node type, etc.
  graph_traversal: yes  # Navigate nodes -> triples -> chunks with provenance tracking
  multi_hop_queries: |
    Natural language queries traverse the graph to find related triples and nodes.
    The query engine embeds triples and retrieves via semantic similarity, pulling
    in linked nodes, properties, and chunks. Multi-hop is implicit via relationship
    traversal, not explicit graph query syntax.
  query_language: custom  # Natural language + structured queries, no Cypher/GraphQL query input
  full_text_search: yes  # Via MongoDB's text search capabilities
  vector_search: yes  # Core feature - chunks and triples are embedded for semantic retrieval

scale:
  documented_limits: |
    No hard limits documented. Relies on MongoDB Atlas scalability. Recommends
    M10+ dedicated clusters for best performance; M30+ for production workloads.
    MongoDB Atlas supports automatic storage scaling at 90% capacity.
  concurrent_access: |
    Multi-user supported in enterprise version. Self-hosted supports multiple
    users via API keys. No explicit documentation on concurrent write handling
    or optimistic locking.
  known_performance_issues: |
    M10/M20 clusters may experience degraded performance under sustained loads.
    Working set should fit in RAM for optimal query performance.

hosting:
  hosted_option: yes  # Enterprise cloud-hosted version available
  hosted_pricing: contact_sales  # No published pricing; contact team@whyhow.ai
  self_host_complexity: moderate
  self_host_requirements: |
    - Python 3.10+
    - MongoDB Atlas account (M10+ cluster recommended)
    - OpenAI API key (for embeddings and generation)
    - Environment variables for MongoDB connection and API keys
    - Optional: Docker (Dockerfile included but not primary deployment method)
    - CLI setup: python admin.py setup-collections
  data_export: yes  # export_cypher method exports graphs to Cypher format

real_time:
  sync_mechanism: polling  # SDK uses poll_interval for async operations (default 5s)
  latency: |
    No real-time push. Async operations (like document upload) use polling with
    configurable timeout (default 120s) and poll_interval (default 5s).
  conflict_resolution: |
    Not documented. MongoDB handles document-level atomicity but no explicit
    CRDT or OT mechanisms for collaborative editing. Enterprise version has
    "multiplayer" features but conflict handling not specified.

agent_integration:
  mcp_tools_available: none  # No MCP server implementation
  langchain_integration: yes  # Demonstrated with LLMGraphTransformer
  llamaindex_integration: partial  # Referenced in docs but not native integration
  other_integrations: |
    - Python SDK (whyhow package on PyPI)
    - OpenAI API (embeddings + generation)
    - Knowledge Table (parallel tool for triple creation)
    - Export to Cypher for Neo4j/other graph databases
    - Async/await support for all SDK methods
```

### Phase 2 Research Notes

**Strengths for Agent Orchestration:**
1. Purpose-built for RAG/agent workflows with "small graph philosophy"
2. Vector chunks as first-class citizens enable context-aware retrieval
3. Semantic triple embedding (not Text2Cypher) claims 2x accuracy improvement
4. Flexible schema supports both exploratory and constrained graph building
5. Export to Cypher enables integration with traditional graph databases
6. Python SDK with full async support

**Limitations for Task Management Use Case:**
1. No native task/todo functionality - "task" collection is for API operation tracking
2. No MCP server - would need to build custom integration
3. No real-time sync (polling only)
4. No built-in dependency modeling for work items
5. Conflict resolution not documented
6. Enterprise features (collaboration) require contacting sales

**Key Technical Findings:**
- Query engine embeds triples (not just nodes) for richer semantic search
- 11 MongoDB collections provide well-structured data organization
- API-first design with Swagger UI documentation at /docs endpoint
- All sync methods have async counterparts
- Cypher export enables data portability to Neo4j/Memgraph

### Additional Phase 2 Sources
- [WhyHow SDK API Documentation](https://whyhow-ai.github.io/whyhow-sdk-docs/api/)
- [Querying Knowledge Graphs](https://docs.whyhow.ai/docs/user-interface/querying/)
- [MongoDB Atlas Service Limits](https://www.mongodb.com/docs/atlas/reference/atlas-limits/)
- [MongoDB Supercharge AI Blog Post](https://www.mongodb.com/blog/post/supercharge-ai-data-management-knowledge-graphs)
