# TerminusDB

```yaml
tool_name: TerminusDB
category: Graph Database
official_url: https://terminusdb.org/
pricing: open_source
pricing_details: |
  Core database is Apache 2.0 licensed open source.
  TerminusCMS (cloud-hosted) offers free tier with premium packages available.
  DFRNT.com provides hosted instances with free tier and commercial upgrades.
  Enterprise services and consultancy available through DataChemists.
platforms:
  web_ui: yes  # Dashboard included, accessible at localhost:6363/dashboard
  desktop: no  # Server-based, runs via Docker or source install on Windows/macOS/Linux
  mobile: no
api:
  exists: yes
  type: [REST, GraphQL, WOQL]
  read_write: both
open_source: yes
repo_url: https://github.com/terminusdb/terminusdb
last_commit: December 2025  # v12.0.2 released December 16, 2025
github_stars: 3200
screening_result: PASS
fail_reason: null

additional_notes: |
  ## Overview
  TerminusDB is a distributed graph database with a "git for data" collaboration model.
  It combines document store capabilities with semantic knowledge graph features,
  allowing JSON documents to be linked in an RDF-based knowledge graph.

  ## Architecture and Performance
  - Written primarily in Prolog (69%), with Rust (8.6%) for the underlying triple store
  - Uses succinct data structures and delta encoding for low memory overhead
  - Claims only 13.57 bytes per triple
  - In-memory database optimized for performance

  ## Query Languages
  - GraphQL: Auto-generated from schema, supports deep linking and path queries
  - WOQL (Web Object Query Language): Datalog-based query language
  - REST API: Full OpenAPI specification available
  - Closed-world RDF: Special-purpose RDF implementation

  ## Version Control Features (Git-like)
  - Branch, clone, merge, push, pull operations
  - Commits for every update with complete audit logs
  - Time-travel queries to any historical state
  - Diff between any commits
  - Work in parallel branches

  ## Client SDKs
  - Python client: pip install terminusdb-client
  - JavaScript client: npm install @terminusdb/terminusdb-client
  - Community Rust client available

  ## AI/LLM Integration
  - VectorLink: Semantic indexer for TerminusCMS
  - Vector database using HNSW graphs (written in Rust)
  - Uses OpenAI for embeddings with GraphQL + Handlebars templates
  - Supports RAG, semantic search, clustering, and entity resolution
  - Currently cloud-only (VectorLink), plans to open-source

  ## Web UI
  - Built-in dashboard at /dashboard endpoint
  - Features: data product management, schema modeling, query testing
  - Note: Local dashboard marked as deprecated/buggy in some docs
  - DFRNT Studio recommended for full modeler UI (works with localhost)

  ## Installation
  - Docker container (recommended): works on Windows, macOS, Linux
  - Source install: requires SWI-Prolog, Rust, clang, make
  - Cloud options: TerminusCMS (Azure), DFRNT.com hosting

  ## MCP Server
  No dedicated MCP server found during research.
  Would need custom integration via REST/GraphQL APIs.

  ## Real-time Capabilities
  No native GraphQL subscriptions or real-time streaming found.
  Designed more for versioned/batch data workflows than real-time.

  ## Strengths for AI Agent Orchestration
  - Strong versioning/provenance tracking for data lineage
  - Multiple query interfaces (GraphQL, REST, WOQL) for flexibility
  - VectorLink for semantic search and RAG use cases
  - Schema-first approach with type system and constraints
  - Could track agent state/context across sessions via branches

  ## Considerations
  - Dashboard UI reportedly deprecated/buggy (DFRNT Studio recommended)
  - No native real-time/subscription support
  - Smaller community than Neo4j/ArangoDB
  - VectorLink AI features are cloud-only currently

additional_sections:
  version_control_for_data:
    description: |
      Unique selling point - native revision control for data products.
      Enables branching, merging, and time-travel queries similar to git.
      Could be valuable for tracking AI agent decision history and rollback.

  semantic_web_support:
    rdf: yes
    owl: no
    sparql: no  # Uses WOQL instead
    json_ld: yes  # Document format uses JSON-LD-like syntax

  cloud_hosting:
    terminuscms: "Azure-hosted managed service"
    dfrnt: "SaaS with modeler, visualizations, and hosting"
    self_hosted: "Docker or source install"
```

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://terminusdb.org/docs/
  auth_model: api_key  # Also supports JWT tokens and OAuth2 via user forward header
  rate_limits: not documented  # No explicit rate limits found in documentation
  webhooks: no  # No webhook support found
  subscriptions: no  # No GraphQL subscriptions or real-time push notifications
  mcp_server: none  # No official or community MCP server found
  mcp_server_url: null

data_model:
  node_structure: |
    Documents are JSON-LD typed objects with a schema-defined @type.
    Each document has a unique IRI identifier. Documents can contain
    subdocuments (nested objects internal to a document's identity).
    Data stored as RDF triples (subject-predicate-object) under the hood,
    but exposed as JSON documents through the Document API.
  schema_flexibility: dynamic  # Schema required but can be modified; JSON-LD typed
  custom_fields: yes  # Properties defined in schema, @metadata for arbitrary JSON
  relations: |
    Links between documents via IRI references in properties.
    Supports foreign typed identifiers to reference external documents.
    RDF-based graph structure enables rich relationship modeling.
    Subdocuments are contained within parent documents (not shared).

task_support:
  native_tasks: no  # No built-in task/work item concept
  status_field: workaround  # Can model status as a property in custom schema
  assignee_field: workaround  # Can model assignee as a property in custom schema
  due_date: workaround  # Can model due_date as a property in custom schema
  dependencies: workaround  # Can model via document links in custom schema
  dependency_description: |
    No native dependency support. Would need to model tasks as documents
    with properties like "blockedBy" as array of task IRIs. Dependencies
    would be queryable via WOQL path queries or GraphQL traversal.
  query_ready_tasks: workaround  # Can query via WOQL for tasks where blockedBy is empty

query_capabilities:
  simple_filters: yes
  graph_traversal: yes  # Strong graph traversal via WOQL path queries
  multi_hop_queries: |
    Excellent support via WOQL datalog. Use unification to chain triple patterns:
    triple("v:Subject", "v:Pred1", "v:Intermediate"), triple("v:Intermediate", "v:Pred2", "v:Object")
    Path queries with regex-like syntax: path(start, "(rel>)+", end) for recursive traversal.
    Can express "find all tasks blocked by tasks owned by X" naturally in WOQL.
  query_language: [graphql, woql, rest]  # WOQL is datalog variant, GraphQL auto-generated
  full_text_search: partial  # Regex supported via regexp(); full-text search is open GitHub issue
  vector_search: yes  # VectorLink for TerminusCMS (cloud only) - uses OpenAI embeddings

scale:
  documented_limits: |
    In-memory database - limited by available RAM.
    Designed for tens of millions of edges.
    13.57 bytes per triple (succinct data structures).
    No hard limits documented; depends on memory.
  concurrent_access: |
    Multi-user collaboration via git-like branching model.
    Users work on branches and merge changes.
    Not designed for high-concurrency real-time writes.
  known_performance_issues: |
    Large schemas can cause performance issues (GitHub issue #33).
    Supernodes (highly connected nodes) can be problematic as with other RDF engines.
    In-memory requirement means large datasets need significant RAM.

hosting:
  hosted_option: yes
  hosted_pricing: |
    TerminusCMS: Free tier available, premium packages exist (specific pricing not publicly documented)
    DFRNT.com: Free version available, SaaS with hosting, self-hosted option
    Contact vendors for enterprise/detailed pricing
  self_host_complexity: easy  # Docker container is primary installation method
  self_host_requirements: |
    Docker (recommended) or source install.
    Minimum 2GB RAM (Windows default), more recommended for larger databases.
    For source: SWI-Prolog, Rust, clang, make required.
    HTTPS reverse proxy needed for network deployment (security).
  data_export: yes  # JSON dump, RDF/Turtle dump supported

real_time:
  sync_mechanism: |
    No WebSocket or real-time push.
    Git-like pull/push for synchronization between nodes.
    Polling via REST API would be required for real-time use cases.
  latency: not documented  # No real-time latency metrics available
  conflict_resolution: |
    Git-inspired merge model with JSON diff/patch.
    Manual conflict resolution UI available.
    Can view both versions and choose original, new, or custom resolution.
    Fix-up queries allow schema compliance before commit.
    Exploring CRDT-style merge-tolerant data structures.

agent_integration:
  mcp_tools_available: null  # No MCP server exists
  langchain_integration: no  # No official integration found
  llamaindex_integration: no  # No official integration found
  other_integrations: |
    Python client: pip install terminusdb-client (WOQLClient, WOQLQuery)
    JavaScript client: npm install @terminusdb/terminusdb-client
    Community Rust client available
    VectorLink (cloud): OpenAI embeddings for RAG/semantic search
    REST API: OpenAPI spec available for custom integrations
```

### Phase 2 Research Notes

**Strengths for Agent Use:**
- Powerful WOQL datalog queries enable complex multi-hop graph traversal
- Path queries with regex-like syntax for recursive relationship navigation
- Version control (commits, branches, time-travel) excellent for provenance tracking
- VectorLink provides semantic search / RAG capabilities (cloud only)
- Schema enforcement ensures data integrity
- Multiple query interfaces (GraphQL, WOQL, REST) offer flexibility

**Limitations for Agent Use:**
- No MCP server - would require custom REST/GraphQL integration
- No real-time subscriptions or webhooks - agents would need to poll
- No native task/workflow support - would need custom schema design
- VectorLink AI features are cloud-only currently
- In-memory architecture requires careful capacity planning
- Smaller community than Neo4j/ArangoDB

**Task Dependency Modeling Approach:**
```woql
# Example: Find all tasks with no blockers
WOQL.and(
  WOQL.triple("v:Task", "rdf:type", "@schema:Task"),
  WOQL.not(WOQL.triple("v:Task", "blockedBy", "v:Blocker"))
)

# Example: Find all tasks blocked by tasks owned by "agent-1"
WOQL.and(
  WOQL.triple("v:Task", "blockedBy", "v:BlockingTask"),
  WOQL.triple("v:BlockingTask", "owner", "agent-1")
)
```
