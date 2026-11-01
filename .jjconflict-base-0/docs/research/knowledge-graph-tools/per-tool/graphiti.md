# Graphiti (by Zep)

```yaml
tool_name: Graphiti
category: AI-Agent Native
official_url: https://www.getzep.com/product/open-source/
pricing: open_source
pricing_details: |
  Graphiti itself is fully open source (Apache-2.0). Zep Cloud (managed service) offers:
  - Free: 1,000 episodes/month, low rate limits
  - Flex: $25/month, 20,000 credits, 600 req/min, 5 projects
  - Flex Plus: $475/month, 300,000 credits, 1,000 req/min, webhooks, API logs
  - Enterprise: Custom pricing, SOC2, HIPAA, BYOK/BYOM/BYOC options
  Credits: 1 episode = 1 credit (episodes >350 bytes billed in multiples)
platforms:
  web_ui: yes  # Zep Cloud has web dashboard at app.getzep.com
  desktop: no
  mobile: no
api:
  exists: yes
  type: REST  # FastAPI-based REST service, plus MCP server
  read_write: both
open_source: yes
repo_url: https://github.com/getzep/graphiti
last_commit: 2026-01-30  # pushed_at from GitHub API
github_stars: 22419
screening_result: PASS
fail_reason:

additional_notes: |
  ## Overview
  Graphiti is a Python framework for building temporally-aware knowledge graphs, specifically
  designed for AI agents operating in dynamic environments. It powers the memory layer behind
  Zep's agent memory platform.

  ## Key Technical Features
  - **Temporal Awareness**: Bi-temporal data model tracking both event occurrence and ingestion times
  - **Real-Time Incremental Updates**: Immediate data integration without batch recomputation
  - **Hybrid Search**: Combines semantic embeddings, BM25 keyword search, and graph traversal
  - **Sub-100ms retrieval latency** (P95: ~300ms for complex queries)

  ## Database Support
  - Neo4j 5.26
  - FalkorDB 1.1.2
  - Kuzu 0.11.2
  - Amazon Neptune

  ## LLM Provider Support
  - OpenAI (default)
  - Anthropic
  - Google Gemini
  - Groq
  - Azure OpenAI

  ## MCP Server
  Graphiti has an official MCP (Model Context Protocol) server enabling integration with:
  - Claude Desktop
  - Cursor IDE
  - Other MCP-compatible AI assistants

  MCP capabilities: episode management, entity management, semantic/hybrid search,
  group management, graph maintenance.

  ## Deployment Options
  1. **Self-hosted**: pip install graphiti-core + your own graph database
  2. **REST Service**: Built-in FastAPI server in /server directory
  3. **MCP Server**: Docker-based deployment for AI assistant integration
  4. **Zep Cloud**: Managed service with web dashboard, SDKs (Python, TypeScript, Go)

  ## Benchmark Performance
  According to Zep's research paper (arXiv:2501.13956), Graphiti/Zep outperforms MemGPT
  on Deep Memory Retrieval benchmarks with up to 18.5% accuracy improvements and 90%
  latency reduction.

  ## Web UI Details
  Zep Cloud (app.getzep.com) provides:
  - Project management and API key management
  - User/Session/Collection viewing and editing
  - Message history and data enrichment viewing
  - Debug logs for ingestion process
  - Webhook management with activity logs and replay
  - Usage monitoring

  ## Framework Integrations
  Works with LangChain, LlamaIndex, AutoGen, and other agent frameworks.

additional_sections:
  agent_memory_focus: |
    Graphiti is purpose-built for AI agent memory, not general knowledge management.
    Key differentiators:
    - Designed for continuous, incremental updates from agent interactions
    - Supports multi-tenant isolation via group_id
    - Optimized for conversational context retrieval
    - Research-backed architecture published in peer-reviewed paper

  real_time_capabilities: |
    - Immediate ingestion without batch recomputation
    - Sub-200ms retrieval latency (Zep Cloud P95: 189ms)
    - Event-driven updates via webhooks (Flex Plus and above)

  enterprise_readiness: |
    Zep Cloud offers:
    - SOC 2 Type II certification
    - HIPAA BAA available (Enterprise)
    - Bring Your Own Key (BYOK)
    - Bring Your Own Model (BYOM)
    - Bring Your Own Cloud (BYOC) - AWS, GCP, Azure VPC deployment
```

## Sources

- [GitHub Repository](https://github.com/getzep/graphiti)
- [Zep Official Website](https://www.getzep.com/)
- [Zep Pricing](https://www.getzep.com/pricing/)
- [Graphiti Open Source Page](https://www.getzep.com/product/open-source/)
- [Research Paper: Zep Temporal Knowledge Graph Architecture](https://arxiv.org/abs/2501.13956)
- [MCP Server Documentation](https://help.getzep.com/graphiti/getting-started/mcp-server)
- [Zep Web Admin UI Announcement](https://blog.getzep.com/announcing-zep-v0-12-0-and-our-new-web-admin-ui/)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://help.getzep.com/graphiti/
  auth_model: api_key  # OpenAI/Anthropic/etc API keys for LLM providers; Zep Cloud uses project API keys
  rate_limits: |
    Self-hosted: Controlled by SEMAPHORE_LIMIT env var (default: 10 concurrent operations)
    Zep Cloud:
    - Free: Low, variable based on service load
    - Flex: 600 requests/minute
    - Flex Plus: 1,000 requests/minute
    - Enterprise: Custom guaranteed limits
  webhooks: yes  # Zep Cloud Flex Plus and above; events include episode.processed, ingest.batch.completed
  subscriptions: no  # No WebSocket subscriptions; uses webhooks for event notifications
  mcp_server: official
  mcp_server_url: https://github.com/getzep/graphiti/tree/main/mcp_server

data_model:
  node_structure: |
    EntityNode structure:
    - uuid: Unique identifier
    - name: Entity name
    - group_id: Multi-tenant namespace isolation
    - labels: Entity type labels (e.g., Entity, Person, Organization)
    - created_at: Timestamp of creation
    - summary: LLM-generated entity summary
    - attributes: Custom attributes from Pydantic models
    - name_embedding: Vector embedding for semantic search

    EpisodeNode structure:
    - Represents ingested data units (text, messages, JSON)
    - Maintains data provenance linking to source episodes
  schema_flexibility: dynamic  # Custom entity/edge types via Pydantic models; schema can evolve over time
  custom_fields: yes  # Define via Pydantic models; protected fields: uuid, name, group_id, labels, created_at, summary, attributes, name_embedding
  relations: |
    EntityEdge (fact/relationship) structure:
    - uuid, source_node_uuid, target_node_uuid
    - name: Relationship type (e.g., 'HELD_POSITION', 'LOVES')
    - fact: Natural language description of the relationship
    - fact_embedding: Vector for semantic search
    - episodes: List of source episode UUIDs
    - Temporal fields: valid_at, invalid_at, created_at, expired_at

    Bi-temporal model enables:
    - valid_at: When fact became true in real world
    - invalid_at: When fact ceased being true (null if still valid)
    - created_at: When ingested into system
    - expired_at: When superseded by new information

task_support:
  native_tasks: no  # Graphiti is designed for agent memory/knowledge, not task management
  status_field: no
  assignee_field: no
  due_date: no
  dependencies: workaround  # Could model task dependencies as graph relationships, but not native
  dependency_description: |
    Graphiti does not have native task management. However, tasks could be modeled as:
    - EntityNode with custom "Task" entity type via Pydantic
    - Dependencies as edges between task nodes (e.g., BLOCKS, DEPENDS_ON)
    - Status/assignee/dates as custom attributes
    This would require custom implementation; not out-of-box functionality.
  query_ready_tasks: no  # No native task queries; would require custom Cypher via underlying database

query_capabilities:
  simple_filters: yes  # Filter by group_id, entity types, edge types via SearchFilters
  graph_traversal: yes  # Native graph traversal combined with semantic search; center node reranking by graph distance
  multi_hop_queries: |
    Limited native support. Multi-hop queries possible via:
    - Center node reranking (rerank by graph distance to specific node)
    - Direct Cypher queries to underlying Neo4j/FalkorDB database
    - Custom retrieval using SearchConfig with graph traversal recipes
    Complex multi-hop queries like "tasks blocked by tasks owned by X" would require
    direct database queries, not high-level Graphiti API.
  query_language: cypher  # Underlying databases use Cypher (Neo4j, FalkorDB); Graphiti provides Python API abstraction
  full_text_search: yes  # BM25 keyword search via underlying database full-text indexes
  vector_search: yes  # Semantic similarity search using embeddings (OpenAI text-embedding-3-small default)

scale:
  documented_limits: |
    No explicit maximum limits documented.
    Claims "near-constant time access to nodes and edges, regardless of graph size"
    via vector and BM25 indexes.
    Zep Cloud: "No limit on number of graphs" and "no limit on graph size"
    Multi-tenant support: FalkorDB supports 10,000+ isolated graph instances
  concurrent_access: |
    - SEMAPHORE_LIMIT controls concurrent episode processing (default: 10)
    - Multi-agent support via FalkorDB tenant isolation (dedicated graph per agent)
    - Zep Cloud handles concurrent episode processing sequentially per graph
    - Recommended: Run Graphiti as separate service for multi-agent workloads
  known_performance_issues: |
    - Episodes >350 bytes billed as multiples (affects cost, not performance)
    - Sequential processing of concurrent episodes to same graph can cause delays
    - 429 rate limit errors from LLM providers if SEMAPHORE_LIMIT too high
    - Some users report rate limiting with ~10,000 characters across chunks

hosting:
  hosted_option: yes  # Zep Cloud at app.getzep.com
  hosted_pricing: |
    - Free: 1,000 credits/month
    - Flex: $25/month (20,000 credits, then $25 per 20,000)
    - Flex Plus: $475/month (300,000 credits, then $125 per 100,000)
    - Enterprise: Custom pricing
  self_host_complexity: moderate
  self_host_requirements: |
    - Python 3.10+
    - Graph database: Neo4j 5.26+, FalkorDB 1.1.2+, Kuzu 0.11.2+, or Amazon Neptune
    - LLM API access: OpenAI (default), Anthropic, Gemini, Groq, or Azure OpenAI
    - Embedding service: OpenAI, Voyage, Sentence Transformers, or Gemini
    - Docker (optional): Compose files for Neo4j/FalkorDB deployments
    - For Amazon Neptune: OpenSearch Serverless required for full-text search
  data_export: yes  # Via underlying database export tools (Neo4j export, FalkorDB migration scripts)

real_time:
  sync_mechanism: |
    - Webhooks (Zep Cloud Flex Plus+): HTTP POST on episode.processed, ingest.batch.completed
    - No WebSocket or CRDT support
    - Immediate graph updates on episode ingestion (no batch recomputation)
  latency: |
    - Retrieval: Sub-100ms typical, P95 ~300ms for complex queries
    - Zep Cloud P95: 189ms retrieval latency
    - FalkorDB integration: Sub-10ms queries claimed
  conflict_resolution: |
    Temporal model handles conflicting information:
    - New facts mark old facts as invalid (invalid_at timestamp)
    - Historical context preserved; current validity clearly indicated
    - LLM-based entity resolution for duplicate detection
    - FalkorDB tenant isolation eliminates update conflicts between agents
    No traditional CRDT or OT conflict resolution.

agent_integration:
  mcp_tools_available: |
    Official MCP server tools:
    - add_episode: Add text/message/JSON episodes to knowledge graph
    - search_nodes: Search for relevant entity node summaries
    - search_facts: Search for relevant facts/edges (semantic + hybrid)
    - get_episodes: Retrieve episodes by reference
    - delete_episode: Remove episode and related data
    - Group management: Organize data with group_id
    - Graph maintenance: Clear graphs, rebuild indices
  langchain_integration: yes  # Official example at examples/langgraph-agent/; community langchain-graphiti package
  llamaindex_integration: yes  # Documented integration; works with LlamaIndex retrievers
  other_integrations: |
    - AutoGen: Supported
    - CrewAI: Via Zep integration
    - MCP-compatible assistants: Claude Desktop, Cursor IDE
    - n8n: Zep credentials integration
    - FalkorDB GraphRAG-SDK
    - Custom agent frameworks via Python SDK
```

## Phase 2 Sources

- [Graphiti GitHub Repository](https://github.com/getzep/graphiti)
- [Graphiti MCP Server README](https://github.com/getzep/graphiti/blob/main/mcp_server/README.md)
- [Zep Documentation - Graphiti](https://help.getzep.com/graphiti/)
- [Zep Pricing Page](https://www.getzep.com/pricing/)
- [Custom Entity and Edge Types Documentation](https://help.getzep.com/graphiti/core-concepts/custom-entity-and-edge-types)
- [Searching the Graph Documentation](https://help.getzep.com/graphiti/working-with-data/searching)
- [Zep Webhooks Documentation](https://help.getzep.com/webhooks)
- [Neo4j Blog - Graphiti Knowledge Graph Memory](https://neo4j.com/blog/developer/graphiti-knowledge-graph-memory/)
- [FalkorDB - Graphiti Integration for Multi-Agent Systems](https://www.falkordb.com/blog/graphiti-falkordb-multi-agent-performance/)
- [langchain-graphiti Package](https://github.com/dev-mirzabicer/langchain_graphiti)
- [Graphiti Rate Limits Discussion (GitHub Issue #290)](https://github.com/getzep/graphiti/issues/290)
