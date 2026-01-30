# Top Candidates: Phase 2 Rankings

Based on the Phase 2 deep evaluation, tools are ranked against the original requirements:

1. **Task orchestration + knowledge accumulation** (equally important)
2. **Real-time collaborative** (multiple agents simultaneously)
3. **Dynamic schemas**
4. **Graph traversal queries**
5. **Scale to 100k+ nodes**

---

## Scoring Methodology

Each requirement scored 0-2:
- **2** = Fully meets requirement
- **1** = Partially meets / workaround available
- **0** = Does not meet

**Max score: 10 points**

---

## Tier 1: Best Fit (Score 7+)

### 1. Neo4j — Score: 8/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | No native tasks, but flexible schema models both well |
| Real-time | 1 | CDC + Kafka for push; no native WebSocket |
| Dynamic Schema | 2 | Fully dynamic labeled property graph |
| Graph Traversal | 2 | Cypher is industry-leading for graph queries |
| Scale 100k+ | 2 | Billions of nodes proven in production |

**Strengths:** Industry standard, 4 official MCP servers, LangChain/LlamaIndex integration, vector search, mature ecosystem.

**Weaknesses:** No native task primitives, real-time requires CDC setup, self-hosting needs moderate expertise.

**Recommendation:** Best choice if you need powerful graph queries and AI framework integration. Model tasks as nodes with BLOCKS/DEPENDS_ON relationships.

---

### 2. Fibery — Score: 8/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 2 | Native tasks with dependencies + flexible entity types |
| Real-time | 1 | Webhooks only, no WebSocket subscriptions |
| Dynamic Schema | 2 | Fully dynamic types and relations |
| Graph Traversal | 1 | GraphQL with nested queries, but not true graph DB |
| Scale 100k+ | 2 | No documented limits, cloud-hosted |

**Strengths:** Official MCP server, native task dependencies with "blocked by" relations, GraphQL API, built-in AI, query ready tasks via `isEmpty` filter.

**Weaknesses:** SaaS only (no self-host), 3 req/sec rate limit, no LangChain integration.

**Recommendation:** Best choice if task management is primary need. Query unblocked tasks directly. Pair with a graph DB for complex knowledge queries.

---

### 3. Dgraph — Score: 7/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | No native tasks, custom schema required |
| Real-time | 2 | Native GraphQL subscriptions via WebSocket |
| Dynamic Schema | 2 | Flexible predicates with optional typing |
| Graph Traversal | 2 | DQL + GraphQL with recursive queries |
| Scale 100k+ | 2 | Billions of edges, horizontal scaling |

**Strengths:** Real-time subscriptions (unique among graph DBs), native GraphQL, Apache 2.0 license, vector search.

**Weaknesses:** No LangChain/LlamaIndex integration, Linux-only server, community MCP only.

**Recommendation:** Best choice if real-time push notifications are critical. Model tasks as nodes, use subscriptions to notify agents of changes.

---

### 4. ArangoDB — Score: 7/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | No native tasks, custom schema required |
| Real-time | 0 | No native change streams (major gap) |
| Dynamic Schema | 2 | Schema-less with optional validation |
| Graph Traversal | 2 | AQL with variable-depth traversal |
| Scale 100k+ | 2 | 1.1M writes/sec benchmark, production-proven |

**Strengths:** Multi-model (graph + doc + vector), official LangChain integration, powerful AQL query language.

**Weaknesses:** No real-time subscriptions (must poll), BSL license.

**Recommendation:** Best choice if you need graph + vector search in one system and can tolerate polling for updates.

---

## Tier 2: Strong Contenders (Score 5-6)

### 5. Graphiti — Score: 6/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | Purpose-built for knowledge, not tasks |
| Real-time | 1 | Webhooks (paid tier), no subscriptions |
| Dynamic Schema | 2 | Custom Pydantic entity types |
| Graph Traversal | 1 | Hybrid search, but limited multi-hop API |
| Scale 100k+ | 1 | Millions supported, but LLM bottleneck |

**Strengths:** Purpose-built for AI agent memory, official MCP, temporal awareness, LangChain/LlamaIndex integration.

**Weaknesses:** Not designed for task management, multi-hop queries require direct Cypher, sequential episode processing.

**Recommendation:** Excellent as knowledge/memory layer. Pair with a task management tool (Linear, Fibery, Beads).

---

### 6. Linear — Score: 6/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | Excellent tasks, limited knowledge storage |
| Real-time | 1 | Webhooks, no subscriptions |
| Dynamic Schema | 1 | Custom fields on paid plans |
| Graph Traversal | 0 | Single-hop only via API |
| Scale 100k+ | 2 | Cloud-hosted, scales well |

**Strengths:** Official MCP, native task dependencies, excellent UX, GraphQL API.

**Weaknesses:** SaaS only, limited to task/project data, no graph queries.

**Recommendation:** Best pure task management option. Query blocked-by relations via API. Pair with graph DB for knowledge.

---

### 7. Beads — Score: 6/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | Excellent tasks, not a knowledge graph |
| Real-time | 0 | Git-based, no push notifications |
| Dynamic Schema | 0 | Fixed ~40 fields |
| Graph Traversal | 1 | Dependency traversal only |
| Scale 100k+ | 0 | Degrades at ~200 active tasks |

**Strengths:** Purpose-built for AI agents, `bd ready` returns unblocked tasks instantly, 19 dependency types, official MCP.

**Weaknesses:** Not a knowledge graph, scale limitations, no custom fields.

**Recommendation:** Best for small-scale AI agent task coordination. Use alongside a knowledge graph.

---

### 8. Cognee — Score: 5/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | Knowledge-focused, no task support |
| Real-time | 0 | SSE for responses only, no sync |
| Dynamic Schema | 2 | Pydantic-based DataPoints |
| Graph Traversal | 2 | Cypher + NL queries, multi-hop |
| Scale 100k+ | 0 | ~1GB/40min processing, gaps at TB scale |

**Strengths:** Graph + vector hybrid, official MCP, LangChain/LlamaIndex integration, 92.5% multi-hop accuracy.

**Weaknesses:** Not for task management, concurrency requires server-based backend, no real-time sync.

**Recommendation:** Excellent knowledge/RAG layer. Not suitable as task backbone.

---

### 9. AFFiNE — Score: 5/10

| Requirement | Score | Notes |
|-------------|-------|-------|
| Task + Knowledge | 1 | Good knowledge, basic tasks without dependencies |
| Real-time | 2 | CRDT-based, conflict-free concurrent edits |
| Dynamic Schema | 1 | Block-based, but not true graph |
| Graph Traversal | 0 | Hierarchical only, no graph queries |
| Scale 100k+ | 1 | Unknown limits, CRDT overhead |

**Strengths:** CRDT for safe concurrent agent edits, community MCP with 36 tools, MIT license, self-hostable.

**Weaknesses:** No task dependencies, no graph traversal, not designed for agent orchestration.

**Recommendation:** Good for collaborative document workspace. Not suitable as task orchestration backbone.

---

## Tier 3: Specialized Use Cases (Score <5)

| Tool | Score | Best For |
|------|-------|----------|
| Atomic-Server | 4 | Real-time sync + easy self-hosting (alpha status) |
| Roam Research | 4 | Datalog queries for personal knowledge (no self-host) |
| SiYuan | 4 | Self-hosted PKM with SQL queries (single-user) |
| Notion | 4 | Task management + docs (no graph traversal) |
| TerminusDB | 4 | Version-controlled data (no MCP, no real-time) |
| Trilium Notes | 3 | Self-hosted hierarchical notes (single-user) |
| LinkedDataHub | 3 | SPARQL queries on RDF data (complex auth) |
| JanusGraph | 3 | Massive scale graphs (complex setup, no MCP) |
| Knowledge Graph Studio | 3 | RAG pipelines (no MCP, limited docs) |
| Coda | 2 | Doc + spreadsheet hybrid (rate limits) |
| Heptabase | 2 | Visual thinking (limited API) |
| Capacities | 1 | Personal PKM (single-user, limited API) |
| Reflect | 1 | Encrypted notes (write-only API) |
| Mem | 1 | AI-powered notes (limited API) |

---

## Recommended Architecture

Based on Phase 2 findings, no single tool perfectly meets all requirements. The recommended approach is a **two-layer architecture**:

### Layer 1: Task Orchestration
**Primary:** Fibery or Linear
- Native task dependencies with blocking relations
- Query "ready tasks" (no blockers) via API
- Official MCP servers
- Webhooks for change notifications

**Alternative:** Beads (if scale <200 tasks, prefer open source)

### Layer 2: Knowledge Graph
**Primary:** Neo4j
- Cypher for complex graph traversal
- LangChain/LlamaIndex integration
- Vector search for semantic queries
- Billions of nodes

**Alternative:** ArangoDB (if need multi-model) or Dgraph (if need real-time subscriptions)

### Integration Pattern
```
Agent → MCP → Task Layer (Fibery/Linear)
              ↓ webhook
        Knowledge Layer (Neo4j)
              ↓ query
Agent ← MCP ← Context/Memory
```

---

## Open Questions for Hands-On Testing

1. **Fibery + Neo4j sync latency** — How fast can task changes propagate to knowledge graph?
2. **Neo4j CDC setup complexity** — Is Kafka required for real-time, or can we use simpler polling?
3. **Dgraph subscription reliability** — How do WebSocket subscriptions perform under agent load?
4. **Beads scale limits** — At what point does performance degrade unacceptably?
5. **AFFiNE CRDT behavior** — How do 10+ concurrent agents affect performance?
