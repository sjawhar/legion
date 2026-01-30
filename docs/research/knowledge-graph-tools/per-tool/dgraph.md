# Dgraph - Phase 1 Screening

```yaml
tool_name: Dgraph
category: Graph Database
official_url: https://dgraph.io/
pricing: freemium
pricing_details: |
  - Open source self-hosted: Free (Apache 2.0 license)
  - Dgraph Cloud Free tier: $0/month (limited)
  - Dgraph Cloud Shared: $39.99/month
  - Dgraph Cloud Dedicated: $199/month (enterprise features, multi-tenancy, audit logs)
  - Enterprise license available for self-hosted with support SLAs
  - Data transfer: 5GB included, $2 per additional GB
platforms:
  web_ui: yes  # Ratel - web-based data visualization and cluster management
  desktop: no  # Server runs on Linux only; no dedicated desktop client app
  mobile: no   # No native mobile app; backend database only
api:
  exists: yes
  type: [GraphQL, DQL, gRPC, REST]
  read_write: both
open_source: yes
repo_url: https://github.com/dgraph-io/dgraph
last_commit: 2026-01-29  # v25.2.0 release
github_stars: 21500
screening_result: PASS
fail_reason:

additional_notes: |
  ## Overview
  Dgraph is a horizontally scalable, distributed GraphQL database with a graph backend.
  Originally built to use GraphQL as a query language, it developed its own Dgraph Query
  Language (DQL) which extends GraphQL with graph-specific capabilities. Now under
  stewardship of Istari Digital (acquired from Hypermode in Oct 2025).

  ## Key Technical Features
  - ACID transactions with consistent replication and linearizable reads
  - Native GraphQL database with automatic schema-to-API generation
  - Dual query language support: GraphQL (standard) and DQL (extended)
  - Full-text search, regex, and geospatial queries built-in
  - Protocol Buffers over gRPC and HTTP (JSON responses)
  - Written in Go, optimized for Linux/amd64 and Linux/arm64

  ## API Details
  - GraphQL endpoint at /graphql with full query/mutation/subscription support
  - DQL for advanced graph traversal beyond GraphQL spec limitations
  - gRPC and HTTP endpoints available
  - Client libraries: Go (dgo), JavaScript (dgraph-js), Python (pydgraph), Java, C#

  ## Web UI (Ratel)
  - Web-based data visualizer and cluster manager
  - Available at play.dgraph.io or self-hosted via Docker
  - Features: DQL query execution, schema management, cluster monitoring, backups
  - GitHub: https://github.com/dgraph-io/ratel

  ## Real-Time Capabilities
  - GraphQL subscriptions via WebSocket (wss:// protocol)
  - Uses subscription-transport-ws protocol
  - @withSubscription directive enables selective subscription on types
  - Supports permessage-deflate compression (since v21.03)

  ## MCP Server Availability
  - Community MCP server: https://github.com/johnymontana/dgraph-mcp-server
    - Tools: dgraph_query, dgraph_mutate, dgraph_alter_schema
    - Exposes schema as MCP resource
    - Built with mcp-go library
  - Native MCP support added in Dgraph core (feat #9389)
  - Listed in Google's MCP Toolbox for Databases

  ## AI Agent Integration Potential
  - Strong: Native GraphQL makes it easy to generate queries from natural language
  - MCP servers available for direct LLM tool integration
  - Subscriptions enable real-time data updates to agents
  - Well-documented client libraries for all major languages
  - JSON responses are easily parsed by LLMs

  ## Production Readiness
  - v25.x is production-ready
  - Used by Fortune 500 companies
  - Notable deployments: Intuit Katlas, VMware Purser
  - Designed for terabyte-scale real-time use cases

  ## Limitations
  - Server only runs on Linux (Mac/Windows support dropped in 2021)
  - No dedicated desktop or mobile client apps
  - Ratel UI is functional but basic compared to some competitors
  - Learning curve for DQL beyond standard GraphQL

additional_sections:
  mcp_integration:
    available: yes
    official: no  # Community-maintained, though native support recently added
    server_url: https://github.com/johnymontana/dgraph-mcp-server
    google_toolbox: yes  # Included in Google's MCP Toolbox for Databases

  real_time_support:
    subscriptions: yes
    protocol: WebSocket (subscription-transport-ws)
    compression: yes

  query_languages:
    - GraphQL (standard)
    - DQL (Dgraph Query Language - extended GraphQL fork)

  client_sdks:
    - language: Go
      repo: https://github.com/dgraph-io/dgo
      stars: 381
    - language: JavaScript
      repo: https://github.com/dgraph-io/dgraph-js
      stars: 337
    - language: Python
      repo: https://github.com/dgraph-io/pydgraph
      stars: 289
    - language: Java
      official: yes
    - language: C#
      official: yes
```

## Sources
- [Dgraph GitHub Repository](https://github.com/dgraph-io/dgraph)
- [Dgraph Documentation](https://docs.dgraph.io/)
- [Dgraph Pricing](https://dgraph.io/pricing)
- [Ratel UI Documentation](https://docs.dgraph.io/ratel/)
- [Ratel GitHub](https://github.com/dgraph-io/ratel)
- [GraphQL Subscriptions](https://dgraph.io/docs/graphql/subscriptions/)
- [Dgraph MCP Server](https://github.com/johnymontana/dgraph-mcp-server)
- [Google MCP Toolbox for Databases](https://googleapis.github.io/genai-toolbox/resources/sources/dgraph/)
- [Client Libraries](https://docs.dgraph.io/clients/)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://docs.dgraph.io/
  auth_model: jwt  # JWT-based with support for Auth0, Cognito, Firebase; also ACL system
  rate_limits: not documented  # No explicit rate limits in docs; Dgraph Cloud may have undocumented limits
  webhooks: no
  subscriptions: yes  # GraphQL subscriptions via WebSocket
  mcp_server: community  # Community MCP server + Google MCP Toolbox support
  mcp_server_url: https://github.com/johnymontana/dgraph-mcp-server

data_model:
  node_structure: |
    Graph-based with predicates (edges/properties) attached to nodes (UIDs).
    Nodes are identified by UIDs (unique identifiers). Predicates can be scalar
    values (string, int, float, bool, datetime, geo, password) or edges to other nodes.
    Supports RDF triples natively. Types can be defined to group predicates.
  schema_flexibility: dynamic  # Schema can be altered at runtime; types are optional
  custom_fields: yes  # Any predicate can be added dynamically
  relations: |
    Edges are first-class citizens. Directed edges connect nodes via predicates.
    Reverse edges can be auto-generated with @reverse directive.
    Edges can have facets (key-value metadata on edges).
    Multi-valued edges supported natively.

task_support:
  native_tasks: no  # General-purpose graph DB, not task-specific
  status_field: workaround  # Can model with custom predicate (e.g., Task.status: string)
  assignee_field: workaround  # Can model with edge (e.g., Task.assignee -> User)
  due_date: workaround  # Can model with datetime predicate (e.g., Task.dueDate: datetime)
  dependencies: workaround  # Can model with edges (e.g., Task.blockedBy -> Task)
  dependency_description: |
    Dependencies can be modeled as edges between Task nodes. For example:
    - Task.blockedBy: [uid] @reverse - creates blockedBy and auto-generates blocks
    - Can traverse dependency chains with recursive queries in DQL
    - No built-in dependency resolution; must implement in application logic
  query_ready_tasks: yes  # Can query tasks with no incoming blockedBy edges using DQL

query_capabilities:
  simple_filters: yes  # term, exact, fulltext, regexp, trigram indexes
  graph_traversal: yes  # Native graph traversal with variable-depth recursion
  multi_hop_queries: |
    DQL supports multi-hop queries natively. Example:
    { tasks(func: type(Task)) @filter(eq(status, "blocked")) {
        blockedBy { owner { name } } } }
    Can find "tasks blocked by tasks owned by X" with nested traversal.
    Supports @recurse directive for variable-depth traversal.
  query_language: graphql+dql  # GraphQL for standard ops, DQL for advanced graph queries
  full_text_search: yes  # Built-in with 15 language support, stemming, stopwords
  vector_search: yes  # Added in v24; float32vector type with HNSW index (euclidean, cosine, dotproduct)

scale:
  documented_limits: |
    - Tested with 2 billion edges (Stack Overflow dataset)
    - Freebase dataset: 1.9 billion RDF triples (250GB uncompressed)
    - Cloud tiers: Shared (25GB), Dedicated (1TB), Enterprise (custom)
    - Users have explored 10 billion node graphs
  concurrent_access: |
    - Distributed ACID transactions with snapshot isolation
    - Optimistic concurrency control (transactions abort on conflict)
    - Multiple replicas per shard for read scaling
    - Horizontal scaling via sharding across Dgraph Alpha nodes
  known_performance_issues: |
    - High fan-out nodes (millions of edges from single node) can be slow
    - Write performance degrades with very large posting lists
    - Memory pressure at extreme scale (billions of goroutines)
    - Data distribution by predicate can cause hotspots

hosting:
  hosted_option: yes
  hosted_pricing: |
    - Free: Limited features, trial
    - Shared: $39.99/month (25GB storage, 5GB transfer)
    - Dedicated: $219/month per node (1TB, HA, ACL, multi-tenancy)
    - Enterprise: Custom pricing (unlimited, network isolation)
  self_host_complexity: moderate
  self_host_requirements: |
    - Docker Desktop 4.37.1+ or Kubernetes
    - Minimum: 1 Zero (cluster manager) + 1 Alpha (data server)
    - Production: 3+ Zeros, 3+ Alphas for HA
    - SSD storage recommended (high I/O)
    - Go 1.24+ for building from source
    - Linux only (Mac/Windows not supported)
  data_export: yes  # RDF and JSON formats; exports schema + data

real_time:
  sync_mechanism: WebSocket (subscription-transport-ws protocol)
  latency: not documented  # Real-time but no SLA on latency
  conflict_resolution: |
    Optimistic concurrency with snapshot isolation.
    Concurrent transactions writing same edge: later commit aborts with ErrConflict.
    Client must retry aborted transactions.
    Read snapshots are consistent across cluster.
    Monotonically increasing timestamps ensure serializability.

agent_integration:
  mcp_tools_available: |
    - dgraph_query: Execute DQL queries with variables
    - dgraph_mutate: Execute mutations with optional commit
    - dgraph_alter_schema: Modify database schema
    - dgraph://schema resource: Retrieve current schema
  langchain_integration: no  # Feature requested (issue #11533) but not implemented
  llamaindex_integration: no  # No official integration; would need custom graph store
  other_integrations: |
    - Google MCP Toolbox for Databases (official support)
    - Hypermode Modus (semantic search with Dgraph)
    - Official client SDKs: Go (dgo), Python (pydgraph), JavaScript (dgraph-js), Java, C#
    - Auth integrations: Auth0, Cognito, Firebase via JWT
```

## Phase 2 Sources
- [Dgraph Documentation](https://docs.dgraph.io/)
- [Dgraph Security](https://docs.dgraph.io/graphql/security/)
- [JWT Authentication](https://docs.dgraph.io/graphql/security/jwt/)
- [GraphQL Subscriptions](https://dgraph.io/docs/graphql/subscriptions/)
- [ACID Transactions](https://dgraph.io/docs/design-concepts/transactions-concept/)
- [Consistency Model](https://docs.hypermode.com/dgraph/concepts/consistency-model)
- [Vector Similarity Search](https://docs.dgraph.io/learn/howto/similarity-search/)
- [Dgraph Scaling Blog](https://dgraph.io/blog/post/scaling-dgraph/)
- [Scale Discussion](https://discuss.dgraph.io/t/is-dgraph-suitable-for-large-scale-ingestion-and-querying-at-billions-of-nodes-and-edges/19668)
- [Dgraph Cloud Pricing](https://dgraph.io/pricing)
- [Single Host Setup](https://docs.hypermode.com/dgraph/self-managed/single-host-setup)
- [Kubernetes Setup](https://docs.dgraph.io/installation/ha-cluster/ha-cluster-k8s-kind/)
- [Data Export](https://dgraph.io/docs/howto/exportdata/about-export/)
- [Dgraph MCP Server](https://github.com/johnymontana/dgraph-mcp-server)
- [Google MCP Toolbox - Dgraph](https://googleapis.github.io/genai-toolbox/resources/sources/dgraph/)
- [LangChain Dgraph Issue](https://github.com/langchain-ai/langchain/issues/11533)
- [pydgraph Client](https://github.com/dgraph-io/pydgraph)
