# Neo4j - Phase 1 Screening

```yaml
tool_name: Neo4j
category: Graph Database
official_url: https://neo4j.com/
pricing: freemium
pricing_details: |
  Neo4j offers multiple pricing tiers:

  **AuraDB (Fully Managed Cloud):**
  - Free: $0 - Learn and explore graphs, no credit card required
  - Professional: Starting at $65/GB/month (min 1GB), hourly billing ~$0.09-$5.76/hour
  - Business Critical: Starting at $146/GB/month (min 2GB), 99.95% SLA, 24x7 support
  - Virtual Dedicated Cloud: Custom pricing, VPC isolation, dedicated infrastructure

  **Self-Managed:**
  - Community Edition: Free, open source (GPLv3)
  - Enterprise Edition: Commercial license, contact sales for pricing

  **Neo4j Desktop:**
  - Free for development, includes Developer edition license (Enterprise features for local dev)

platforms:
  web_ui: yes  # Neo4j Browser - full-featured web UI for queries, visualization, administration
  desktop: Windows/macOS/Linux  # Neo4j Desktop 2.x available on all major platforms
  mobile: no  # Only deprecated community projects (Android v0.1 from ~2014, no official apps)

api:
  exists: yes
  type: [REST, GraphQL, Bolt, HTTP]
  read_write: both
  details: |
    - **Query API (HTTP)**: Execute Cypher queries via HTTP (port 7474/7473)
    - **Bolt Protocol**: Binary protocol for high-performance driver connections
    - **GraphQL Library**: Open source library for auto-generating GraphQL APIs from schema
    - **Official Drivers**: Python, Java, .NET, JavaScript, Go
    - **Community Drivers**: Ruby, PHP, Perl, Rust

open_source: yes  # Community Edition is GPLv3
repo_url: https://github.com/neo4j/neo4j
last_commit: 2026-01 (approximate - active development, release/5.26.0 branch)
github_stars: 15800  # ~15.8k stars

screening_result: PASS
fail_reason: null

additional_notes: |
  ## MCP Server Availability (Strong AI/Agent Support)

  Neo4j has official MCP (Model Context Protocol) servers maintained by Neo4j Labs:
  - **mcp-neo4j-cypher**: Execute Cypher queries via natural language
  - **mcp-neo4j-memory**: Personal knowledge graph for cross-session memory
  - **mcp-neo4j-cloud-aura-api**: Manage Aura instances from AI assistants
  - **mcp-neo4j-data-modeling**: Create/validate graph data models

  MCP servers have 885 stars, 231 forks, actively maintained (latest release Dec 2025).
  Works with Claude Desktop, VS Code, Cursor, Windsurf.

  ## Real-Time Capabilities

  - **Change Data Capture (CDC)**: Track changes to nodes/relationships in real-time
  - **Kafka Connector**: Official Neo4j Connector for Apache Kafka (source & sink)
  - **Streaming**: Event-driven architecture support via CDC + Kafka
  - **CDC Modes**: FULL (complete before/after state) and DIFF (changes only)

  ## Query Language

  - **Cypher**: Declarative graph query language, industry standard (openCypher)
  - **APOC**: Extensive library of procedures (1,849 GitHub stars)
  - **Graph Data Science (GDS)**: Built-in algorithms for analytics

  ## Neo4j Aura (Hosted Cloud)

  - Available on AWS, Azure, Google Cloud (60+ regions)
  - Pay-per-capacity with hourly metering
  - Pause database feature saves 80% costs
  - No extra charges for storage, compute, IO, network, or backups
  - Enterprise features available in Business Critical tier

  ## Neo4j Browser (Web UI)

  - Monaco editor (VS Code engine) for Cypher queries
  - D3.js-based graph visualization
  - Multiple result views: Graph, Table, Raw JSON
  - Built-in guides and tutorials
  - Browser Sync for cross-device settings
  - Accessible at localhost:7474 (HTTP) or :7473 (HTTPS)

  ## Agent/Orchestration Relevance

  - Excellent for knowledge graph memory in AI agents
  - MCP integration allows natural language graph operations
  - GraphRAG patterns well-documented
  - Neo4j GraphAcademy offers free MCP tools course
  - LangChain, CrewAI, Pydantic.AI, Google ADK support Neo4j MCP

  ## Ecosystem Maturity

  - 10+ years of development
  - Large community (2,529 repos tagged "neo4j" on GitHub)
  - Extensive documentation and GraphAcademy courses
  - Strong enterprise adoption

additional_sections:
  cypher_query_language:
    type: Declarative graph query language
    standardization: openCypher (open standard)
    features:
      - Pattern matching
      - Graph traversal
      - CRUD operations
      - Aggregations
      - Subqueries

  graph_algorithms:
    library: Graph Data Science (GDS)
    categories:
      - Centrality (PageRank, Betweenness, etc.)
      - Community detection (Louvain, Label Propagation)
      - Pathfinding (Dijkstra, A*)
      - Similarity (Node Similarity, Jaccard)
      - Machine Learning (embeddings, link prediction)

  vector_search:
    available: yes
    version_requirement: Neo4j 5.15+
    features:
      - Native vector indexing
      - Similarity search
      - GraphQL @vector directive support

  enterprise_features:
    - Role-based access control (RBAC)
    - SSO authentication
    - Clustering and high availability
    - Online backups
    - Point-in-time recovery
    - Audit logging
```

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://neo4j.com/docs/
  auth_model: other  # Basic auth, OAuth2 (Aura API), Bolt native auth, LDAP, SSO (Enterprise)
  rate_limits: |
    - Authentication rate limiting: Blocks after too many failed login attempts
    - Aura API /oauth/token: Rate limited as of Jan 2025; tokens valid for 1 hour
    - Transaction limit: Default 1000 concurrent transactions (configurable via db.transaction.concurrent.maximum)
    - Recommended: Cache OAuth tokens, reuse for 1 hour to avoid rate limits
  webhooks: no  # No native webhook support; use CDC + Kafka for event streaming
  subscriptions: no  # No native subscriptions; real-time via CDC procedures or Kafka
  mcp_server: official
  mcp_server_url: https://github.com/neo4j/mcp

data_model:
  node_structure: |
    Neo4j implements a labeled property graph model:
    - **Nodes**: Represent entities; have unique IDs, zero or more labels, and key-value properties
    - **Labels**: Classify nodes into named subsets (e.g., :Person, :Task); nodes can have multiple labels
    - **Relationships**: Named, directed connections between nodes; must have exactly one type; can have properties
    - **Properties**: Key-value pairs on nodes and relationships; support strings, numbers, booleans, arrays
  schema_flexibility: dynamic  # Schema-free by default; optional constraints and indexes can be added later
  custom_fields: yes  # Properties are arbitrary key-value pairs; no predefined schema required
  relations: |
    - Relationships are first-class citizens with their own properties
    - Always directed (one direction) but can be traversed bidirectionally in queries
    - Must have exactly one type (e.g., BLOCKS, DEPENDS_ON, ASSIGNED_TO)
    - Support properties like weight, timestamp, metadata
    - Index-free adjacency: Each node stores pointers to its relationships for O(1) traversal

task_support:
  native_tasks: no  # Neo4j is a general-purpose graph DB; tasks must be modeled as nodes
  status_field: no  # No built-in status; model as node property (e.g., task.status = "pending")
  assignee_field: no  # No built-in assignee; model as relationship (e.g., -[:ASSIGNED_TO]->)
  due_date: no  # No built-in due date; model as node property (e.g., task.due_date)
  dependencies: workaround
  dependency_description: |
    Model task dependencies using relationships:
    - Create relationship types like :BLOCKS, :DEPENDS_ON, :PREREQUISITE_OF
    - Example: (taskA)-[:BLOCKS]->(taskB) means taskB is blocked by taskA
    - Multi-hop dependency chains naturally supported via graph traversal
    - Cypher pattern matching finds all upstream/downstream dependencies
  query_ready_tasks: yes
  query_ready_tasks_description: |
    Query tasks with no blockers using Cypher:
    ```cypher
    // Find all tasks with no incomplete blockers
    MATCH (t:Task {status: 'pending'})
    WHERE NOT EXISTS {
      MATCH (blocker:Task)-[:BLOCKS]->(t)
      WHERE blocker.status <> 'completed'
    }
    RETURN t
    ```

query_capabilities:
  simple_filters: yes  # WHERE clauses with property comparisons, regex, IN lists
  graph_traversal: yes  # Core strength; pattern matching with variable-length paths
  multi_hop_queries: |
    Powerful multi-hop queries via pattern matching:
    ```cypher
    // Find all tasks blocked by tasks owned by person X
    MATCH (p:Person {name: 'Alice'})<-[:OWNED_BY]-(blocker:Task)-[:BLOCKS*1..]->(blocked:Task)
    RETURN DISTINCT blocked

    // Find all upstream dependencies (any depth)
    MATCH path = (t:Task {id: 'task-123'})<-[:BLOCKS*]-(upstream:Task)
    RETURN upstream, length(path) as depth

    // Quantified path patterns (Neo4j 5.x)
    MATCH (start:Task)-[:BLOCKS]->{1,5}(end:Task)
    RETURN start, end
    ```
  query_language: cypher  # Declarative graph query language, openCypher standard
  full_text_search: yes  # Apache Lucene-powered fulltext indexes; db.index.fulltext.queryNodes()
  vector_search: yes  # HNSW indexes (Neo4j 5.15+); cosine/euclidean similarity; native vector indexing

scale:
  documented_limits: |
    - Neo4j 3.0+ removed all hard limits via dynamic pointer compression
    - Theoretical: 64-bit addressing allows practically unlimited nodes/relationships
    - Practical limits determined by hardware (RAM, storage, CPU)
    - Successfully tested with billions of nodes in production
    - Bloom visualization tool limited to 10,000 nodes (UI limit, not DB limit)
  concurrent_access: |
    - Default: 1000 concurrent transactions (configurable)
    - ACID compliant with transaction isolation
    - Locks ensure node/relationship consistency during concurrent modifications
    - Bookmarks enable causal consistency across cluster nodes
    - Read-committed isolation level by default; simulate serializable with explicit locks
    - Clustering: Read replicas for horizontal read scaling
  known_performance_issues: |
    - Supernodes (high-degree nodes) can slow traversals; use relationship type filtering
    - Complex multi-MATCH queries may have unexpected execution plans; use PROFILE to debug
    - Memory pressure with very large aggregations; configure heap appropriately
    - Vertical scaling primarily; horizontal write scaling limited to single leader

hosting:
  hosted_option: yes
  hosted_pricing: |
    - **AuraDB Free**: $0, limited resources, no credit card
    - **AuraDB Professional**: Starting $65/GB/month (~$0.09/hour), hourly billing
    - **AuraDB Business Critical**: Starting $146/GB/month, 99.95% SLA, 24x7 support
    - **Virtual Dedicated Cloud**: Custom pricing, VPC isolation
    - No extra charges for storage, compute, IO, network, or backups
    - Pause feature saves ~80% costs during inactive periods
  self_host_complexity: moderate
  self_host_requirements: |
    - **Docker**: Official images on DockerHub (Community and Enterprise editions)
    - **Kubernetes**: Helm charts for deployment; supports AWS EKS, GKE, AKS
    - **Requirements**: Java Runtime, sufficient RAM (recommend 100GB+ for large graphs)
    - **Windows**: WSL 2.0 required
    - **Enterprise**: Requires license agreement acceptance
    - **APOC plugin**: Recommended for extended functionality
  data_export: yes
  data_export_formats: |
    - **neo4j-admin dump/backup**: Full database export (native format)
    - **APOC export**: CSV, JSON, GraphML, Cypher script
    - **LOAD CSV**: Import from CSV files
    - **Cypher RETURN**: Query results as JSON via HTTP API
    - **Bulk import**: neo4j-admin import for initial large dataset loading

real_time:
  sync_mechanism: |
    - **Change Data Capture (CDC)**: Transaction log-based change tracking
    - **Kafka Connector**: Official Neo4j Connector for Apache Kafka (source & sink)
    - **CDC Procedures**: db.cdc.query, db.cdc.current, db.cdc.earliest for polling
    - No native WebSocket or push notifications
  latency: |
    - CDC captures changes in real-time from transaction log
    - Kafka integration provides near-real-time streaming
    - Polling-based approach adds latency proportional to poll interval
  conflict_resolution: |
    - ACID transactions prevent conflicts via locking
    - Concurrent writes to same node/relationship are serialized
    - Optimistic concurrency via conditional Cypher (WHERE exists checks)
    - Cluster: Causal consistency with bookmarks; leader handles writes

agent_integration:
  mcp_tools_available: |
    **Official Neo4j MCP Server** (https://github.com/neo4j/mcp):
    - **get-schema**: Introspect labels, relationship types, property keys
    - **read-cypher**: Execute read-only Cypher queries (blocks writes/admin)
    - **write-cypher**: Execute arbitrary Cypher (dev environments only)
    - **list-gds-procedures**: List Graph Data Science procedures

    **Neo4j Labs MCP Servers** (https://github.com/neo4j-contrib/mcp-neo4j):
    - **mcp-neo4j-cypher**: Natural language to Cypher translation
    - **mcp-neo4j-memory**: Persistent knowledge graph memory for agents
    - **mcp-neo4j-cloud-aura-api**: Manage Aura instances
    - **mcp-neo4j-data-modeling**: Create/validate graph data models

    **Transport modes**: STDIO (default) or HTTP (multi-tenant/web)
    **Read-only mode**: NEO4J_READ_ONLY env var disables write tools
  langchain_integration: yes  # Neo4jGraph wrapper, CypherQAChain, GraphTransformers, hybrid search
  llamaindex_integration: yes  # Graph store, KnowledgeGraphQueryEngine, text2cypher, MCP Tool Spec
  other_integrations: |
    - CrewAI: MCP server support
    - Pydantic.AI: MCP integration
    - Google ADK (Agent Development Kit): MCP support
    - Official drivers: Python, Java, JavaScript, .NET, Go
    - Community drivers: Ruby, PHP, Perl, Rust
    - GraphQL: Auto-generated APIs from schema
```

### Phase 2 Research Sources

- [Neo4j Authentication Documentation](https://neo4j.com/docs/operations-manual/current/authentication-authorization/)
- [Neo4j Aura API Authentication](https://neo4j.com/docs/aura/classic/platform/api/authentication/)
- [Neo4j Official MCP Server](https://github.com/neo4j/mcp)
- [Neo4j Labs MCP Servers](https://github.com/neo4j-contrib/mcp-neo4j)
- [Neo4j MCP Developer Guide](https://neo4j.com/developer/genai-ecosystem/model-context-protocol-mcp/)
- [Neo4j Graph Database Concepts](https://neo4j.com/docs/getting-started/appendix/graphdb-concepts/)
- [Neo4j Data Modeling Tutorial](https://neo4j.com/docs/getting-started/data-modeling/tutorial-data-modeling/)
- [Neo4j Cypher Graph Traversal](https://graphacademy.neo4j.com/courses/cypher-intermediate-queries/4-graph-traversal/01-graph-traversal/)
- [Neo4j CDC Documentation](https://neo4j.com/docs/cdc/current/)
- [Neo4j Vector Indexes](https://neo4j.com/docs/cypher-manual/current/indexes/semantic-indexes/vector-indexes/)
- [Neo4j Full-Text Indexes](https://neo4j.com/docs/cypher-manual/current/indexes/semantic-indexes/full-text-indexes/)
- [Neo4j Transaction Management](https://neo4j.com/docs/operations-manual/current/database-internals/transaction-management/)
- [Neo4j Concurrent Data Access](https://neo4j.com/docs/operations-manual/current/database-internals/concurrent-data-access/)
- [Neo4j Docker Getting Started](https://neo4j.com/docs/operations-manual/current/docker/introduction/)
- [Neo4j Kubernetes Prerequisites](https://neo4j.com/docs/operations-manual/current/kubernetes/quickstart-standalone/prerequisites/)
- [Neo4j LangChain Integration](https://neo4j.com/labs/genai-ecosystem/langchain/)
- [Neo4j LlamaIndex Integration](https://neo4j.com/labs/genai-ecosystem/llamaindex/)
- [APOC Export Documentation](https://neo4j.com/docs/apoc/current/export/)
