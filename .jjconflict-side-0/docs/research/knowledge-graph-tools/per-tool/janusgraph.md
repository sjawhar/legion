# JanusGraph

```yaml
tool_name: JanusGraph
category: Graph Database
official_url: https://janusgraph.org/
pricing: open_source
pricing_details: |
  Completely free under Apache 2.0 license. No commercial licensing fees.
  Costs are limited to infrastructure (servers, storage backends like Cassandra/HBase)
  and operational expenses for managing deployments.
platforms:
  web_ui: yes
  desktop: no
  mobile: no
api:
  exists: yes
  type: other
  read_write: both
  details: |
    - Gremlin query language via Apache TinkerPop
    - WebSocket connections (default, port 8182)
    - REST/HTTP API (via HttpChannelizer configuration)
    - Java client libraries (primary language - 98.9% of codebase)
    - .NET Gremlin Language Variant available
open_source: yes
repo_url: https://github.com/JanusGraph/janusgraph
last_commit: 2024-11-09
github_stars: 5700
screening_result: PASS
fail_reason:

additional_notes: |
  ## Overview
  JanusGraph is a scalable, distributed graph database optimized for storing and
  querying graphs containing hundreds of billions of vertices and edges across
  multi-machine clusters. It's a Linux Foundation project supported by IBM, Google,
  Hortonworks, and Amazon. Fork of the original TitanDB (developed since 2012).

  ## Web UI Options
  - **janusgraph-visualizer**: Official web-based visualization tool (37 GitHub stars)
    - Docker Compose deployment available
    - Features: query visualization, interactive graph exploration, property inspection,
      customizable node labels, query history, cumulative result merging
    - Accessible at http://localhost:3001 when running
  - **GraphExp**: Lightweight D3.js-based web interface for graph exploration
  - **G.V() Gremlin IDE**: Commercial option with schema management, table/graph/JSON views
  - **Third-party tools**: Cytoscape, Gephi, Graphlytic, KeyLines, Ogma, Tom Sawyer

  ## API Architecture
  - Uses Gremlin Server (Apache TinkerPop) as the server component
  - Can run in WebSocket mode (default) or HTTP/REST mode (configurable)
  - Cannot run both modes simultaneously on single instance; requires two instances
  - REST endpoint example: POST to http://host:8182 with {"gremlin":"g.V().count()"}

  ## Storage Backends
  - Apache Cassandra (distributed)
  - Apache HBase (Hadoop ecosystem)
  - Oracle Berkeley DB Java Edition (embedded/local)
  - Google Cloud Bigtable

  ## Index Backends
  - Elasticsearch
  - Apache Solr
  - Apache Lucene

  ## Analytics Integration
  - Apache Spark for OLAP/global graph analytics
  - Apache Giraph integration
  - Apache Hadoop integration

  ## Key Features
  - ACID transactions with support for eventual consistency
  - Thousands of concurrent users
  - Real-time complex graph traversals
  - Property graph model
  - Schema management API for data modeling
  - JSON-based schema definition (v1.1.0+)

  ## AI/Agent Integration
  - No dedicated MCP server found
  - No specific AI agent integrations documented
  - Would require custom MCP server wrapping Gremlin/REST APIs
  - Gremlin query language could be integrated with LLM agents for natural language
    to graph query translation

  ## Deployment Options
  - Standalone server (Java application)
  - Docker container (janusgraph/janusgraph:latest)
  - Kubernetes/Helm charts available
  - Embedded mode (in-process)

  ## Version History
  - Latest: v1.1.0 (November 9, 2024)
  - v1.0.0 marked Apache TinkerPop-enabled property graph milestone
  - 7,381+ commits, 168 contributors

additional_sections:
  query_language:
    name: Gremlin
    type: Graph traversal language
    standard: Apache TinkerPop
    features:
      - Declarative and imperative patterns
      - Complex traversal support
      - Schema management commands
  governance:
    foundation: The Linux Foundation
    license: Apache 2.0
    contributors:
      - IBM
      - Google
      - Hortonworks
      - Amazon
      - Expero
      - GRAKN.AI
  scalability:
    horizontal: yes
    max_vertices: "Hundreds of billions"
    distributed: yes
    clustering: yes
```

## Sources

- [JanusGraph Official Website](https://janusgraph.org/)
- [JanusGraph GitHub Repository](https://github.com/JanusGraph/janusgraph)
- [JanusGraph Documentation](https://docs.janusgraph.org/)
- [JanusGraph-Visualizer Repository](https://github.com/JanusGraph/janusgraph-visualizer)
- [JanusGraph Server Documentation](https://docs.janusgraph.org/v0.3/basics/server/)
- [JanusGraph Releases](https://github.com/JanusGraph/janusgraph/releases)
- [JanusGraph Tools Wiki](https://github.com/JanusGraph/janusgraph/wiki/Tools)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://docs.janusgraph.org/
  auth_model: other  # Supports Basic Auth, SASL (WebSocket), HMAC tokens, Kerberos
  rate_limits: none  # No built-in rate limiting; must implement at API gateway layer
  webhooks: no
  subscriptions: no  # No native real-time subscriptions or CDC
  mcp_server: none
  mcp_server_url: null  # Would require custom implementation wrapping Gremlin API

data_model:
  node_structure: |
    Property graph model via Apache TinkerPop. Vertices have labels, IDs (64-bit auto-assigned),
    and properties. Edges have labels, IDs, and properties. Graph stored in adjacency list format
    where each vertex's incident edges and properties are stored compactly together.
  schema_flexibility: dynamic  # Can be implicit (auto-created) or explicit; recommended to use explicit
  custom_fields: yes  # Property keys with configurable data types and cardinality (SINGLE, LIST, SET)
  relations: |
    Edge labels define relationships with configurable multiplicity constraints:
    - MULTI: Multiple edges of same label between vertex pairs (multi-graph)
    - SIMPLE: At most one edge of label between vertex pairs
    - MANY2ONE, ONE2MANY, ONE2ONE: Directional cardinality constraints
    Vertex labels can be static (immutable after creation transaction).

task_support:
  native_tasks: no  # Generic graph database; tasks must be modeled as vertices
  status_field: no  # Would need custom property key
  assignee_field: no  # Would need custom property key
  due_date: no  # Would need custom property key
  dependencies: workaround  # Model as edges between task vertices
  dependency_description: |
    Dependencies can be modeled as directed edges (e.g., "blocks" or "depends_on" edge labels)
    between task vertices. Edge multiplicity can enforce constraints (e.g., ONE2MANY for
    single-blocker scenarios). Requires custom schema design.
  query_ready_tasks: yes  # Gremlin can traverse to find tasks with no incoming "blocked_by" edges

query_capabilities:
  simple_filters: yes  # has(), hasLabel(), hasId() predicates
  graph_traversal: yes  # Core capability - native graph traversal
  multi_hop_queries: |
    Full support via Gremlin. Example: Find tasks blocked by tasks owned by X:
    g.V().has('owner', 'X').in('blocks').hasLabel('task')
    Supports repeat().until() for variable-depth traversals, path tracking, etc.
  query_language: gremlin  # Apache TinkerPop Gremlin traversal language
  full_text_search: yes  # Via Elasticsearch, Solr, or Lucene index backends
  vector_search: no  # Not natively supported; would require custom implementation

scale:
  documented_limits: |
    - Max edges: 2^60 (quintillion)
    - Max vertices: 2^59 (half of edge limit)
    - Property key types are immutable once committed
    - Reserved keywords: vertex, element, edge, property, label, key
    - Mixed indexes don't support SET/LIST cardinality properties
  concurrent_access: |
    Supports thousands of concurrent users. Thread-safe transactions via ThreadLocal
    or explicit createThreadedTx(). Parallel algorithms supported via thread-independent
    transactions.
  known_performance_issues: |
    - Edge retrieval by ID is O(log(k)) where k = incident edges on adjacent vertex
    - Batch loading slower than single-machine DBs; supernode loading may fail
    - HTTP Gremlin requires Groovy compilation (cached after first request)
    - Locking expensive; can cause deadlocks with many concurrent modifications

hosting:
  hosted_option: no  # IBM Compose for JanusGraph deprecated; no current managed offering
  hosted_pricing: n/a
  self_host_complexity: moderate  # Requires storage backend (Cassandra/HBase) + optional index backend
  self_host_requirements: |
    - Java 11+ runtime
    - Storage backend: Cassandra, HBase, BerkeleyDB, or Google Bigtable
    - Optional index backend: Elasticsearch, Solr, or Lucene
    - Docker: janusgraph/janusgraph:latest
    - Kubernetes: Helm charts available (community-maintained)
    - Typical production: 3+ Cassandra nodes + 1+ JanusGraph servers
  data_export: yes  # GraphML (XML), GraphSON (JSON), Gryo (Kryo binary)

real_time:
  sync_mechanism: none  # No native pub/sub or CDC; WebSocket is for queries only
  latency: null  # Dependent on storage backend and query complexity
  conflict_resolution: |
    - Optional locking via ConsistencyModifier.LOCK on schema elements
    - Locking protocol: re-read, verify, persist, release
    - FORK modifier for edges: concurrent modifications create copies instead of conflicting
    - PermanentLockingException thrown on conflicts
    - Alternative: allow conflicts, resolve at read time
    - Not ACID on Cassandra/HBase by default; configurable on BerkeleyDB

agent_integration:
  mcp_tools_available: |
    None available. Would need to build custom MCP server exposing:
    - Vertex/edge CRUD operations
    - Gremlin query execution
    - Schema management
    - Transaction control
  langchain_integration: no  # No official integration; could use langchain-mcp-adapters with custom server
  llamaindex_integration: no  # No official integration; MCP Tool Spec could consume custom server
  other_integrations: |
    - Apache TinkerPop ecosystem (any Gremlin-compatible client)
    - Java, Python, .NET, JavaScript Gremlin clients
    - Apache Spark for OLAP analytics
    - Visualization: janusgraph-visualizer, GraphExp, G.V(), Gephi, Cytoscape
```

## Phase 2 Sources

- [JanusGraph Documentation](https://docs.janusgraph.org/)
- [JanusGraph Server Documentation](https://docs.janusgraph.org/operations/server/)
- [JanusGraph Schema and Data Modeling](https://docs.janusgraph.org/schema/)
- [JanusGraph Data Model](https://docs.janusgraph.org/advanced-topics/data-model/)
- [JanusGraph Technical Limitations](https://docs.janusgraph.org/advanced-topics/technical-limitations/)
- [JanusGraph Transactions](https://docs.janusgraph.org/basics/transactions/)
- [JanusGraph Eventually-Consistent Storage](https://docs.janusgraph.org/advanced-topics/eventual-consistency/)
- [JanusGraph Index Parameters and Full-Text Search](https://docs.janusgraph.org/index-backend/text-search/)
- [JanusGraph Container Documentation](https://docs.janusgraph.org/operations/container/)
- [JanusGraph Gremlin Query Language](https://docs.janusgraph.org/getting-started/gremlin/)
- [Apache TinkerPop Gremlin](https://tinkerpop.apache.org/gremlin.html)
- [Practical Gremlin Tutorial](https://kelvinlawrence.net/book/Gremlin-Graph-Guide.html)
- [LangChain MCP Adapters](https://github.com/langchain-ai/langchain-mcp-adapters)
- [JanusGraph on GKE with Bigtable](https://cloud.google.com/bigtable/docs/running-janusgraph-with-bigtable)
