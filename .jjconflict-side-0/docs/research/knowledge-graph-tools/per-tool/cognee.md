# Cognee - Phase 1 Screening

```yaml
tool_name: Cognee
category: AI-Agent Native
official_url: https://www.cognee.ai/
pricing: freemium
pricing_details: |
  - Basic (Free): Open source, tasks/pipelines, custom schema/ontology, 28+ data sources
  - Cloud Subscription: $25/month (beta) - hosted platform, multi-tenant, 1GB ingestion + 10k API calls/month
  - On-Prem Subscription: $3,500/month - 1-day SLA, hands-on support, architecture review
  - Enterprise: Custom pricing available
platforms:
  web_ui: yes
  desktop: no
  mobile: no
api:
  exists: yes
  type: REST
  read_write: both
open_source: yes
repo_url: https://github.com/topoteretes/cognee
last_commit: 2026-01-28
github_stars: 11482
screening_result: PASS
fail_reason:

additional_notes: |
  Cognee is a memory engine for AI agents that combines knowledge graphs with vector search.
  It transforms raw data into persistent, queryable AI memory using ECL (Extract, Cognify, Load) pipelines.

  ## Key Technical Features
  - Supports 30+ data sources for ingestion
  - Compatible with multiple vector DBs: LanceDB, Qdrant, PGVector, Weaviate
  - Compatible with multiple graph DBs: Neo4j, NetworkX, kuzu
  - Works with LLM providers: OpenAI, Ollama, Anyscale
  - Python SDK with pip/poetry/uv installation
  - Docker deployment supported

  ## Web UI
  - Local UI launched via `cognee-cli -ui`
  - Interactive notebooks for running cognee methods
  - Graph Explorer for visualizing knowledge graphs
  - Cloud dashboard available with Cognee Cloud subscription

  ## API Endpoints
  - POST /api/add - Add data to knowledge base
  - POST /api/cognify - Transform data into knowledge graphs
  - POST /api/search - Query knowledge graph (natural language or structured)
  - DELETE /api/delete - Remove data
  - GET /api/health - Service health check
  - Authentication via X-Api-Key header (Cloud) or optional for self-hosted

  ## Agent Framework Integrations
  - LangChain integration (langchain-cognee package)
  - LlamaIndex compatible
  - Vercel AI SDK integration (cognee-vercel-ai-sdk)

  ## MCP Server
  - Official MCP (Model Context Protocol) server available since v0.3.5
  - Supports multiple transports: HTTP, SSE, stdio
  - Compatible with Claude Desktop, Cursor, Cline
  - Tools exposed: cognify, codify, search, list_data, delete, prune
  - Can analyze code repositories and build knowledge graphs from codebases

  ## Unique Capabilities
  - Reduces hallucinations by providing structured, contextual knowledge
  - Peer-reviewed research published (arXiv 2505.24478)
  - Modular pipeline architecture with user-defined tasks
  - Automatic entity extraction and relationship mapping via LLM
  - Supports custom ontologies and schemas

additional_sections:
  mcp_server:
    available: yes
    transport_modes: [http, sse, stdio]
    tools: [cognify, codify, search, list_data, delete, prune]
    docker_support: yes
  real_time_capabilities:
    streaming: yes (via SSE transport)
    webhooks: no
  agent_specific_features:
    - Memory persistence across sessions
    - Context-aware retrieval
    - Multi-step task execution with explanations
    - Domain-based memory grouping
    - User-based memory isolation
  supported_databases:
    vector: [LanceDB, Qdrant, PGVector, Weaviate]
    graph: [Neo4j, NetworkX, kuzu]
    relational: [SQLite, PostgreSQL]
  license: Apache-2.0
```

## Sources

- [Cognee Official Website](https://www.cognee.ai/)
- [Cognee GitHub Repository](https://github.com/topoteretes/cognee)
- [Cognee API Documentation](https://docs.cognee.ai/api-reference/introduction)
- [Cognee UI Documentation](https://docs.cognee.ai/how-to-guides/cognee-ui)
- [Cognee MCP Documentation](https://docs.cognee.ai/how-to-guides/cognee-mcp)
- [Cognee Pricing Page](https://www.cognee.ai/pricing)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://docs.cognee.ai/api-reference/introduction
  auth_model: api_key  # X-Api-Key header for Cloud; optional for self-hosted (REQUIRE_AUTH=true to enable)
  rate_limits: Based on subscription plan (Cloud only); no limits for self-hosted
  webhooks: no
  subscriptions: no  # No real-time subscriptions/push mechanism documented
  mcp_server: official
  mcp_server_url: https://github.com/topoteretes/cognee/tree/main/cognee-mcp

data_model:
  node_structure: |
    DataPoints are the core building blocks - strongly typed Pydantic models that serve as both
    node and edge schemas. Each DataPoint includes:
    - id (UUID), created_at, updated_at, version
    - metadata.index_fields (determines which fields are embedded for vector search)
    - type (class name), belongs_to_set (groups related DataPoints)
    Nested DataPoint references automatically create graph edges (e.g., Book.author -> Author creates an edge)
  schema_flexibility: dynamic  # User-defined DataPoint classes with Pydantic validation; custom ontologies supported
  custom_fields: yes  # Full control over DataPoint schema with typed fields
  relations: |
    Relationships are derived from DataPoint references. When a DataPoint field references another DataPoint,
    Cognee automatically creates: Node(Parent), Node(Child), and Edge(Parent -> Child, type=field_name).
    Recursive unpacking handles deeply nested connections. Supports custom relationship types via field definitions.

task_support:
  native_tasks: no  # Cognee is a memory/knowledge engine, not a task management system
  status_field: no  # Not applicable - designed for knowledge storage, not task tracking
  assignee_field: no
  due_date: no
  dependencies: no
  dependency_description: |
    Cognee does not have native task management. It is designed as a memory layer for AI agents
    that stores knowledge graphs and enables semantic retrieval. Pipeline tasks (not user tasks)
    have status tracking (blocking vs background execution), but this is for data processing workflows,
    not project/task management.
  query_ready_tasks: no  # Not applicable

query_capabilities:
  simple_filters: yes  # Via search types and dataset filtering
  graph_traversal: yes  # Multi-hop traversal via Cypher or natural language queries
  multi_hop_queries: |
    Fully supported. GRAPH_COMPLETION search combines vector similarity with graph traversal
    for multi-hop reasoning. Chain-of-thought retriever connects concepts across contexts.
    Example: "Find the manager of the person who approved Project X" is handled via graph paths.
    Reports 92.5% accuracy on multi-hop benchmarks (HotPotQA).
  query_language: cypher  # Native Cypher support (requires Neo4j backend); also supports natural language -> Cypher translation
  full_text_search: no  # Relies on semantic/vector search rather than traditional full-text indexing
  vector_search: yes  # Core feature - embeddings stored in vector DB (LanceDB, Qdrant, PGVector, Weaviate)

scale:
  documented_limits: |
    - Cloud: 1GB ingestion + 10k API calls/month on $25/month plan
    - Processing benchmark: ~1GB in 40 minutes using 100+ containers
    - Acknowledged gap: scaling to terabyte-sized datasets needs further evaluation
    - Docker default limits: 2 CPUs, 4GB memory
  concurrent_access: |
    Depends on storage backend:
    - File-based (Kuzu, SQLite, LanceDB): Limited concurrency; file-level locking. Not suitable for multi-agent.
    - Server-based (Neo4j, PostgreSQL, FalkorDB): High concurrency supported.
    - Recommendation: Use Neo4j or FalkorDB for multi-agent deployments; per-user DB files can mitigate Kuzu limits.
  known_performance_issues: |
    - File-based graph databases (Kuzu) use file locking, limiting concurrent access
    - Chunking is slower than LangChain/LlamaIndex (30x slower than LangChain RecursiveCharacterTextSplitter)
    - Terabyte-scale datasets not fully validated

hosting:
  hosted_option: yes  # Cognee Cloud
  hosted_pricing: |
    - Cloud Subscription: $25/month (beta) - 1GB ingestion, 10k API calls/month
    - On-Prem Support: $3,500/month - 1-day SLA, hands-on support
    - Enterprise: Custom pricing
  self_host_complexity: moderate
  self_host_requirements: |
    - Docker or Docker Compose (recommended)
    - Python 3.9-3.12
    - Storage backends: Choose from file-based (zero infra) or server-based (PostgreSQL, Neo4j)
    - LLM provider API key (OpenAI, Ollama, etc.)
    - Resource defaults: 2 CPUs, 4GB memory
    - Volume mounts for DATA_ROOT_DIRECTORY and SYSTEM_ROOT_DIRECTORY
  data_export: yes  # Data stored in standard databases (PostgreSQL, Neo4j, etc.) which support native export formats

real_time:
  sync_mechanism: |
    - SSE (Server-Sent Events) transport for streaming responses
    - No WebSocket or CRDT support documented
    - Background pipeline execution with status polling (cognify_status, codify_status tools)
  latency: Sub-100ms for complex multi-hop queries (documented benchmark)
  conflict_resolution: |
    Not documented. File-based backends use file-level locking (prevents concurrent writes rather than
    resolving conflicts). For multi-agent scenarios, server-based backends (Neo4j, PostgreSQL) are
    recommended which handle concurrency via their native transaction mechanisms.

agent_integration:
  mcp_tools_available: |
    11 tools exposed via MCP:
    - cognify: Transform data into knowledge graph
    - codify: Analyze code repository, build code graph
    - search: Query memory (GRAPH_COMPLETION, RAG_COMPLETION, CODE, CHUNKS, SUMMARIES, CYPHER, FEELING_LUCKY)
    - list_data: List datasets and data items with IDs
    - delete: Remove data (soft or hard deletion)
    - prune: Reset cognee (remove all data)
    - save_interaction: Log user-agent interactions
    - get_developer_rules: Retrieve generated developer rules
    - cognee_add_developer_rules: Ingest developer rule files
    - cognify_status: Check cognify pipeline progress
    - codify_status: Check codify pipeline progress
  langchain_integration: yes  # langchain-cognee package available
  llamaindex_integration: yes  # Official integration: llama_index.graph_rag.cognee.CogneeGraphRAG
  other_integrations: |
    - Vercel AI SDK (cognee-vercel-ai-sdk)
    - n8n workflow automation
    - Google ADK (Agent Development Kit)
    - 30+ data source connectors
```

## Phase 2 Sources

- [Cognee API Reference](https://docs.cognee.ai/api-reference/introduction)
- [Cognee MCP Documentation](https://docs.cognee.ai/how-to-guides/cognee-mcp)
- [Cognee MCP GitHub](https://github.com/topoteretes/cognee/tree/main/cognee-mcp)
- [Cognee DataPoints Documentation](https://docs.cognee.ai/core-concepts/building-blocks/datapoints)
- [Cognee Search Documentation](https://docs.cognee.ai/core-concepts/main-operations/search)
- [Cognee Deployment Guide](https://docs.cognee.ai/how-to-guides/cognee-sdk/deployment)
- [Cognee Semantic Search Blog](https://www.cognee.ai/blog/deep-dives/the-art-of-intelligent-retrieval-unlocking-the-power-of-search)
- [Cognee LlamaIndex Integration](https://docs.llamaindex.ai/en/stable/examples/graph_rag/llama_index_cognee_integration/)
- [Cognee AI Memory Benchmarks](https://www.cognee.ai/blog/deep-dives/ai-memory-evals-0825)
- [Cognee Self-Hosting Guide](https://www.bitdoze.com/cognee-self-host/)
