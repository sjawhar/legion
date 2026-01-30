# ArangoDB

```yaml
tool_name: ArangoDB
category: Graph Database
official_url: https://arango.ai/
pricing: freemium
pricing_details: |
  - Community Edition: Free and open-source (BSL 1.1 license for v3.12+, Apache 2.0 for v3.11 and earlier)
  - Enterprise Edition: Commercial license, contact for pricing
  - Managed Cloud (AMP): Starting at $0.20/hour, varies by cloud provider, region, disk and CPU size
  - Self-Managed: Run on your own infrastructure (Kubernetes, VMs, bare metal)
  - OEM/Embedded: Integrate directly into your own products
platforms:
  web_ui: yes
  desktop: no  # Server-based, runs via Docker or native install on Linux/Mac/Windows (native installers deprecated as of v3.12)
  mobile: no
api:
  exists: yes
  type: REST
  read_write: both
open_source: yes  # Source-available under BSL 1.1 (converts to Apache 2.0 after 4 years)
repo_url: https://github.com/arangodb/arangodb
last_commit: 2026-01 (active development, 52,289+ commits)
github_stars: 14100
screening_result: PASS
fail_reason:

additional_notes: |
  ## Multi-Model Database
  ArangoDB is a native multi-model database supporting:
  - Graph data (property graph model)
  - JSON documents
  - Key-value pairs
  - Full-text search
  - Geospatial data
  - Vector embeddings (via FAISS integration)

  All models accessible via single query language: AQL (ArangoDB Query Language)

  ## Web Interface Features
  - Dashboard with server statistics
  - AQL query editor with explain functionality
  - Graph visualizer for interactive exploration
  - User management
  - Cluster management for distributed deployments
  - Accessible at http://localhost:8529 by default

  ## HTTP API
  - Full RESTful API following REST principles
  - OpenAPI 3.1 specification available
  - Swagger UI built into web interface
  - Supports JSON and VelocyPack binary format
  - ACID transaction support

  ## AI/Agent Integration
  - **MCP Server Available**: Official ArangoDB MCP server for AI assistants
    - Enables natural language to AQL query generation
    - Schema discovery and query execution
    - Multiple implementations available (TypeScript by ravenwits, Python by PCfVW with 33+ tools)
    - Compatible with Claude Desktop, VS Code extensions (Cline), and other MCP clients
  - **GraphRAG**: Turn-key solution combining vector search, graphs, and LLMs
    - Automatic knowledge graph creation from raw text
    - Entity extraction and relationship mapping
    - HybridGraphRAG: combines vector search, graph traversal, and full-text search
  - **Vector Search**: Native integration via Facebook's FAISS library
  - **Private LLM Support**: Air-gapped deployment option with Triton Inference Server
  - **NVIDIA Partnership**: VSS Blueprint integration for video knowledge graphs

  ## Deployment Options
  - Docker (recommended for all platforms)
  - Linux native packages (Debian, Red Hat)
  - Kubernetes via kube-arangodb operator
  - Cloud managed service (ArangoGraph) on AWS and GCP
  - Native installers deprecated as of v3.12

  ## Enterprise Features
  - SmartGraphs and SmartJoins for optimized query execution
  - EnterpriseGraphs for large-scale graph sharding
  - Full ACID transactions
  - 360-degree encryption
  - LDAP integration
  - GPU acceleration

  ## Licensing Note
  Version 3.12+ uses BSL 1.1 (Business Source License):
  - Source-available, not OSI-approved open source
  - Free for non-commercial and most commercial use
  - Restricted for offering as managed database service
  - Converts to Apache 2.0 after 4 years
  - Drivers, Kubernetes operator, and tools remain Apache 2.0

  ## Rated #1 Graph Database
  ArangoDB was rated the #1 Graph Database in the world for Fall 2025.

additional_sections:
  mcp_integration:
    available: yes
    implementations:
      - name: arango-mcp-server (ravenwits)
        language: TypeScript
        url: https://mcpservers.org/servers/ravenwits/mcp-server-arangodb
      - name: arangodb-mcp-server (PCfVW)
        language: Python
        tools_count: 33+
    features:
      - Natural language to AQL conversion
      - Schema discovery
      - Graph traversal
      - Shortest path queries
      - Document CRUD operations

  real_time_capabilities:
    change_streams: no  # Not a primary feature
    websocket_support: limited
    notes: Primarily REST-based, polling for updates

  graph_features:
    model: Property Graph
    query_language: AQL (ArangoDB Query Language)
    traversal: yes
    shortest_path: yes
    pattern_matching: yes
    graph_algorithms: yes

  vector_search:
    available: yes
    engine: FAISS
    integration: Native via AQL
    status: Available in self-hosted, coming to ArangoGraph cloud

  ai_suite:
    graphrag: yes
    hybrid_retrieval: yes
    knowledge_graph_automation: yes
    private_llm_support: yes
    status: Pre-release, early access available
```

## Sources

- [Arango Official Website](https://arango.ai/)
- [ArangoDB GitHub Repository](https://github.com/arangodb/arangodb)
- [ArangoDB Documentation - Web Interface](https://docs.arangodb.com/3.11/components/web-interface/)
- [ArangoDB HTTP API Documentation](https://docs.arangodb.com/3.12/develop/http-api/)
- [ArangoDB Pricing](https://arango.ai/pricing/)
- [ArangoDB MCP Server Documentation](https://docs.arango.ai/ecosystem/arangodb-mcp-server/)
- [ArangoDB GraphRAG Documentation](https://docs.arangodb.com/3.13/data-science/graphrag/)
- [ArangoDB Licensing Blog Post](https://arango.ai/blog/evolving-arangodbs-licensing-model-for-a-sustainable-future/)
- [ArangoDB Vector Search Blog](https://arango.ai/blog/vector-search-in-arangodb-practical-insights-and-hands-on-examples/)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://docs.arangodb.com/3.12/develop/http-api/
  auth_model: other  # HTTP Basic or JWT token authentication
  auth_details: |
    - HTTP Basic authentication supported
    - JWT token authentication (expires after 1 hour by default, configurable via --server.session-timeout)
    - Authentication enabled by default for database APIs (--server.authentication)
    - Superuser JWT tokens available for admin APIs (requires JWT secret)
  rate_limits: none  # No built-in rate limiting documented; must be implemented at application/proxy level
  webhooks: no
  subscriptions: no  # No native change streams or real-time subscriptions
  mcp_server: community  # Multiple community implementations, official Docker image available
  mcp_server_url: https://hub.docker.com/r/arangodb/mcp-arangodb

data_model:
  node_structure: |
    - Native multi-model: combines graph, document, and key-value in single database
    - Vertices stored as JSON documents in vertex/document collections
    - Edges stored as JSON documents in edge collections with _from/_to attributes
    - Each document has automatic _id, _key, _rev system attributes
    - Edges reference vertices via document IDs (collection/key format)
    - Named graphs vs anonymous graphs for integrity management
  schema_flexibility: freeform  # Schema-less by default; optional JSON Schema validation available
  custom_fields: yes  # Any JSON-compatible fields can be added to documents
  relations: |
    - Edges are full JSON documents linking _from and _to vertices
    - Supports OUTBOUND, INBOUND, and ANY traversal directions
    - Named graphs enforce referential integrity (no dangling edges)
    - Anonymous graphs allow flexible ad-hoc relationships
    - Multiple edge collections can define different relationship types

task_support:
  native_tasks: no  # General-purpose database; tasks must be modeled as documents
  status_field: workaround  # Can add status field to task documents
  assignee_field: workaround  # Can add assignee field to task documents
  due_date: workaround  # Can add due_date field to task documents
  dependencies: workaround  # Model as edges between task documents
  dependency_description: |
    Tasks can be modeled as vertices in a graph with dependency edges:
    - Create "tasks" document collection for task nodes
    - Create "depends_on" edge collection for dependencies
    - Edge from task A to task B means "A depends on B"
    - Use graph traversals to find all blockers/dependents
  query_ready_tasks: yes  # Via AQL graph traversal
  query_ready_tasks_description: |
    Query tasks with no incomplete blockers using AQL:
    ```aql
    FOR task IN tasks
      FILTER task.status != "completed"
      LET blockers = (
        FOR blocker IN 1..1 OUTBOUND task depends_on
          FILTER blocker.status != "completed"
          RETURN blocker
      )
      FILTER LENGTH(blockers) == 0
      RETURN task
    ```

query_capabilities:
  simple_filters: yes  # Full filter support in AQL
  graph_traversal: yes  # Native graph traversal with configurable depth
  multi_hop_queries: |
    Fully supported via AQL traversals with min..max depth syntax:
    - "Find all tasks blocked by tasks owned by X":
      ```aql
      FOR owner_task IN tasks
        FILTER owner_task.assignee == "X"
        FOR blocked IN 1..10 INBOUND owner_task depends_on
          RETURN DISTINCT blocked
      ```
    - Variable depth traversals (e.g., 2..5 hops)
    - Breadth-first or depth-first traversal options
    - Path filtering with PRUNE conditions
  query_language: other  # AQL (ArangoDB Query Language)
  query_language_details: |
    AQL is a declarative SQL-like language supporting:
    - Document CRUD operations
    - Graph traversals (FOR v, e, p IN ... GRAPH)
    - Shortest path and k-shortest paths
    - All shortest paths between two vertices
    - Aggregations, joins, subqueries
    - Array and object manipulation functions
  full_text_search: yes  # ArangoSearch with BM25 ranking, stemming, fuzzy matching
  vector_search: yes  # Native FAISS integration with COSINE_SIMILARITY and L2 distance

scale:
  documented_limits: |
    - AQL query max collections/shards: 2048 (configurable via --query.max-collections-per-query)
    - Expression nesting limit: 500 levels
    - JSON/VelocyPack recursion depth: ~200 levels
    - Transaction size limited by RAM (auto-commits if too large)
    - Transactions cannot be nested
  concurrent_access: |
    - Document-level locking (RocksDB engine)
    - Writes do not block reads; reads do not block writes
    - Local Snapshot Isolation in clusters
    - Causal consistency for dependent transactions
    - Full ACID for single-instance and OneShard deployments
    - Multi-document ACID possible in clusters with OneShard + Stream Transactions
  known_performance_issues: |
    - Complex graph queries challenging for large sharded graphs
    - OLTP-optimized; OLAP workloads may need external tools (e.g., Spark)
    - Deep traversals (50+ depth, 200k+ nodes) can be slow
    - Transaction data stored in RAM; large transactions may auto-commit

hosting:
  hosted_option: yes  # ArangoGraph (Arango Managed Platform)
  hosted_pricing: |
    - Custom pricing via private offer (contact sales)
    - Available on AWS, GCP, Azure
    - Pricing varies by cloud provider, region, disk/CPU size
    - Free trial available
  self_host_complexity: moderate
  self_host_requirements: |
    - Docker: Recommended for all platforms (Docker Desktop 4.37.1+)
    - Kubernetes: kube-arangodb operator, K8s 1.18+
    - Hardware: x86-64 with SSE 4.2 + AVX, or ARM64 (ARMv8 with Neon)
    - Memory: RocksDB cache defaults to 30% of (RAM - 2GB)
    - Port: 8529 default
  data_export: yes  # JSON export via backup tools and AQL

real_time:
  sync_mechanism: polling  # No native change streams
  sync_details: |
    - No native WebSocket or change stream support
    - Community tool "Arangochair" provides change listening (single-node only)
    - Uses Server-Sent Events (SSE) rather than WebSocket
    - Replication API can be used for custom change detection
    - Long-requested feature not yet in core database
  latency: N/A  # Depends on polling interval
  conflict_resolution: |
    - Document-level locking prevents write conflicts
    - _rev attribute for optimistic concurrency control
    - Transactions provide isolation
    - No built-in CRDT or automatic merge for concurrent edits

agent_integration:
  mcp_tools_available: |
    **TypeScript MCP Server (ravenwits)** - 7 tools:
    - arango_query: Execute AQL queries with bind variables
    - arango_insert: Insert documents
    - arango_update: Update documents
    - arango_remove: Delete documents
    - arango_backup: Backup collections to JSON
    - arango_list_collections: List all collections
    - arango_create_collection: Create new collections

    **Python Async MCP Server (PCfVW)** - 46 tools across 11 categories:
    - Multi-Tenancy: set/get focused database, list databases
    - Core Data: query, insert, update, remove, collections
    - Index Management: list, create, delete indexes
    - Query Analysis: explain, query builder, profiling
    - Data Validation: schema creation, document validation
    - Bulk Operations: bulk insert/update
    - Graph Management: create graphs, add vertices/edges, traversal
    - Graph Traversal: traverse, shortest path
    - Graph Backup: backup/restore individual and named graphs
    - Health & Status: database status overview
    - MCP Design Patterns: tool search, workflow management
  langchain_integration: yes  # Official langchain-arangodb package
  langchain_details: |
    - Package: langchain-arangodb (PyPI)
    - ArangoGraph: Database wrapper for graph operations
    - ArangoChatMessageHistory: Store chat history as graph nodes
    - ArangoVector: Vector store with hybrid search (vector + BM25)
    - ArangoGraphQAChain: Natural language to AQL queries
    - Documentation: https://langchain-arangodb.readthedocs.io/
  llamaindex_integration: yes  # Reader available on LlamaHub
  llamaindex_details: |
    - Package: llama-index-readers-arango-db (LlamaHub)
    - SimpleArangoDBReader: Load documents from ArangoDB for RAG
    - Concatenates ArangoDB docs into LlamaIndex Document format
  other_integrations: |
    - Python driver: python-arango
    - JavaScript driver: arangojs
    - GraphRAG: HybridGraphRAG combining vector search, graph traversal, full-text
    - NVIDIA VSS Blueprint: Video knowledge graph integration
    - Spark connector for OLAP workloads
    - Foxx microservices framework for custom APIs
```

## Phase 2 Sources

- [ArangoDB HTTP API Documentation](https://docs.arangodb.com/3.12/develop/http-api/)
- [ArangoDB Authentication](https://docs.arangodb.com/3.11/develop/http-api/authentication/)
- [ArangoDB Graph Traversals](https://docs.arangodb.com/3.12/aql/graphs/traversals/)
- [ArangoDB AQL Limitations](https://docs.arangodb.com/3.11/aql/fundamentals/limitations/)
- [ArangoDB Transaction Limitations](https://www.arangodb.com/docs/stable/transactions-limitations.html)
- [ArangoDB Single vs Cluster Deployments](https://docs.arangodb.com/3.12/deploy/single-instance-vs-cluster/)
- [ArangoDB Docker Installation](https://docs.arangodb.com/3.11/operations/installation/docker/)
- [ArangoDB Kubernetes Operator](https://github.com/arangodb/kube-arangodb)
- [ArangoDB MCP Server (ravenwits)](https://github.com/ravenwits/mcp-server-arangodb)
- [ArangoDB Async MCP Server (PCfVW)](https://github.com/PCfVW/mcp-arangodb-async)
- [LangChain ArangoDB Integration](https://langchain-arangodb.readthedocs.io/)
- [LlamaIndex ArangoDB Reader](https://llamahub.ai/l/readers/llama-index-readers-arango-db)
- [ArangoDB Vector Search Blog](https://arango.ai/blog/vector-search-in-arangodb-practical-insights-and-hands-on-examples/)
- [ArangoSearch Documentation](https://docs.arangodb.com/3.11/index-and-search/arangosearch/)
- [GitHub Issue: Real-time Change Feeds](https://github.com/arangodb/arangodb/issues/602)
