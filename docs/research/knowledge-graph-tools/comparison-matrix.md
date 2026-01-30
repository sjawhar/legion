# Knowledge Graph Tools Comparison Matrix

## Summary

- **Total Tools Evaluated:** 28
- **Passed Phase 1:** 23
- **Failed Phase 1:** 5 (Logseq, Obsidian, Tana, RemNote, Anytype)
- **Passed with Caveats:** 3 (Reflect, Mem, Capacities)

---

## Phase 2: Technical Deep-Dive Matrix

### Critical Capabilities for Agent Orchestration

| Tool | Query Language | Graph Traversal | Task Dependencies | Real-Time | Vector Search | LangChain |
|------|---------------|-----------------|-------------------|-----------|---------------|-----------|
| **Graphiti** | Cypher (via backend) | Yes | No (custom) | Webhooks | Yes | Yes |
| **Cognee** | Cypher/NL | Yes | No | SSE only | Yes | Yes |
| **Beads** | None (CLI) | Limited | **Yes (native)** | No | No | No |
| Knowledge Graph Studio | NL only | Yes | No | No | Yes | Yes |
| **Neo4j** | **Cypher** | **Yes** | No (custom) | CDC/Kafka | Yes | **Yes** |
| **Dgraph** | DQL/GraphQL | **Yes** | No (custom) | **WebSocket** | Yes | No |
| **ArangoDB** | **AQL** | **Yes** | No (custom) | No | Yes | **Yes** |
| Atomic-Server | REST/Paths | Limited | No | **WebSocket** | No | No |
| LinkedDataHub | **SPARQL** | **Yes** | No (custom) | No | No | No |
| TerminusDB | **WOQL** | **Yes** | No (custom) | No | Cloud only | No |
| JanusGraph | **Gremlin** | **Yes** | No (custom) | No | No | No |
| **Linear** | GraphQL | No | **Yes (native)** | Webhooks | No | No |
| **Fibery** | GraphQL | Limited | **Yes (native)** | Webhooks | Yes | No |
| Coda | Formulas | No | Workaround | Webhooks | No | No |
| **AFFiNE** | GraphQL | No | No | **CRDT** | No | No |
| SiYuan | **SQL** | Yes | No | No | No | No |
| Trilium Notes | SQL-like | Limited | No | WebSocket | No | No |
| Notion | REST filters | No | **Yes (native)** | Webhooks | No | Yes |
| Roam Research | **Datalog** | **Yes** | Workaround | No | No | No |
| Heptabase | None | No | No | Sync only | Semantic | No |
| Capacities | None | Limited | No | No | No | No |
| Reflect | None | No | No | CRDT | Semantic | No |
| Mem | None | No | No | No | Yes | No |

### Self-Hosting & Scale

| Tool | Self-Host | Complexity | Scale (nodes) | Concurrent Access | Rate Limits |
|------|-----------|------------|---------------|-------------------|-------------|
| **Graphiti** | Yes | Moderate | Millions+ | Multi-tenant | LLM-bound |
| **Cognee** | Yes | Moderate | ~1GB/40min | File-lock (default) | None |
| **Beads** | Yes | Easy | ~200 active | Git-based | None |
| Knowledge Graph Studio | Yes | Moderate | MongoDB limits | Multi-user | None |
| **Neo4j** | Yes | Moderate | **Billions** | **Yes** | None (self) |
| **Dgraph** | Yes | Moderate | **Billions** | **Yes** | None (self) |
| **ArangoDB** | Yes | Moderate | **Billions** | **Yes** | None (self) |
| Atomic-Server | Yes | **Easy** | Millions | Yes | None |
| LinkedDataHub | Yes | Moderate | Triplestore-dep | ACL-based | None |
| TerminusDB | Yes | Easy | RAM-bound | Yes | None |
| JanusGraph | Yes | Complex | **Quintillions** | Yes | None |
| **Linear** | No | N/A | 250 (free) | Yes | 5000/hr |
| **Fibery** | No | N/A | Unlimited | Yes | 3/sec |
| Coda | No | N/A | 1000 (free) | Yes | **10/6sec** |
| **AFFiNE** | Yes | Moderate | Unknown | **CRDT** | None |
| SiYuan | Yes | **Easy** | Unknown | Single-user | None |
| Trilium Notes | Yes | **Easy** | Unknown | Single-user | None |
| Notion | No | N/A | 10k rows/db | Yes | 3/sec |
| Roam Research | No | N/A | ~10k pages | Yes | Unknown |
| Heptabase | No | N/A | Unknown | Sync only | Unknown |
| Capacities | No | N/A | Unknown | **Single-user** | Unknown |
| Reflect | No | N/A | Unknown | Sync only | Unknown |
| Mem | No | N/A | Unknown | Yes | 100/min |

---

## Phase 1 Details by Category

### AI-Agent Native (4/4 PASS)

| Tool | Stars | MCP Server | API Type | License | Notes |
|------|-------|------------|----------|---------|-------|
| **Graphiti** | 22k | Official | REST | Apache 2.0 | Temporal knowledge graphs, real-time updates, AI agent memory focused |
| **Cognee** | 11.5k | Official | REST | Apache 2.0 | Graph + vector hybrid, 30+ data sources, LangChain integration |
| **Beads** | 13.7k | Official | CLI/JSON | Apache 2.0 | Git-backed issue tracker for AI agents, not general knowledge graph |
| Knowledge Graph Studio | 894 | No | REST | MIT | Multi-agent RAG focused, MongoDB-based, smaller community |

### PKM / Knowledge Graph (9/14 PASS)

| Tool | Stars | MCP Server | API Type | License | Notes |
|------|-------|------------|----------|---------|-------|
| **AFFiNE** | 62k | Community | GraphQL | MIT | CRDT-based, conflict-free concurrent edits, self-hostable |
| **SiYuan** | 41k | Community | REST+SQL | AGPL-3.0 | Block-level model, Docker web UI, Chinese origin |
| **Trilium Notes** | 34k | Community | REST (ETAPI) | AGPL-3.0 | Hierarchical notes, self-hosted web UI |
| Notion | — | **Official** | REST | Proprietary | 21 MCP tools, no native graph visualization |
| Roam Research | — | Community | REST+Datalog | Proprietary | Paid only ($15/mo), powerful Datalog queries |
| Heptabase | — | **Official** | MCP only | Proprietary | MCP is the API, paid only ($9-18/mo) |
| Capacities | — | Community | REST (beta) | Proprietary | Single-user only, limited write capabilities |
| Reflect | — | No | REST | Proprietary | Write-only due to E2E encryption |
| Mem | — | Via Composio | REST | Proprietary | Write-only REST, read via MCP workaround |

### Task/Project Management (3/3 PASS)

| Tool | Stars | MCP Server | API Type | License | Notes |
|------|-------|------------|----------|---------|-------|
| **Linear** | — | **Official** | GraphQL | Proprietary | Freemium (API on free tier), webhooks, TypeScript SDK |
| **Fibery** | — | **Official** | GraphQL | Proprietary | Freemium, webhooks, built-in AI features |
| Coda | — | Community | REST | Proprietary | Freemium, restrictive POST rate limit (10/min) |

### Graph Databases (7/7 PASS)

| Tool | Stars | MCP Server | API Type | License | Notes |
|------|-------|------------|----------|---------|-------|
| **Neo4j** | — | **4 Official** | REST/Bolt/GraphQL | GPLv3 (CE) | Industry leader, Cypher queries, free tier (Aura) |
| **Dgraph** | 21.5k | Community | GraphQL/DQL | Apache 2.0 | Native GraphQL, real-time subscriptions |
| **ArangoDB** | — | Community | REST | BSL 1.1 | Multi-model (graph+doc+vector), native GraphRAG |
| **Atomic-Server** | — | **Built-in** | REST (Atomic Data) | MIT | Native MCP, sub-ms performance, Rust-based |
| LinkedDataHub | 597 | Via Web-Algebra | REST+SPARQL | Apache 2.0 | RDF/Linked Data focused |
| TerminusDB | — | No | REST/GraphQL/WOQL | Apache 2.0 | Git-for-data versioning, time-travel queries |
| JanusGraph | 5.7k | No | Gremlin | Apache 2.0 | Massive scale, Linux Foundation project |

---

## MCP Server Availability Summary

### Official MCP Servers (9 tools)
1. Graphiti (Zep)
2. Cognee
3. Beads
4. Notion
5. Heptabase
6. Linear
7. Fibery
8. Neo4j (4 servers!)
9. Atomic-Server (built-in)

### Community MCP Servers (9 tools)
1. Roam Research
2. AFFiNE
3. SiYuan
4. Trilium Notes
5. Capacities
6. Coda
7. Dgraph
8. ArangoDB
9. LinkedDataHub (via Web-Algebra)

### No MCP Server (5 tools)
1. Knowledge Graph Studio
2. Reflect
3. TerminusDB
4. JanusGraph
5. Mem (Composio workaround only)

---

## Standout Candidates for Phase 2

### For AI Agent Orchestration + Knowledge Storage (Primary Use Case)

**Top Tier:**
1. **Graphiti** — Purpose-built for AI agent memory, official MCP, real-time updates, temporal awareness
2. **Cognee** — Graph + vector hybrid, official MCP, LangChain/LlamaIndex integration
3. **Neo4j** — Industry standard, 4 official MCP servers, vector search, mature ecosystem

**Strong Contenders:**
4. **AFFiNE** — 62k stars, CRDT for concurrent edits, MIT license, self-hostable
5. **Dgraph** — Native GraphQL, real-time subscriptions, Apache 2.0
6. **Atomic-Server** — Native MCP built-in, MIT license, sub-ms performance

### For Task-Focused Orchestration
1. **Beads** — Git-backed issue tracker designed for AI agents
2. **Linear** — Official MCP, GraphQL, excellent UX
3. **Fibery** — Flexible data model, official MCP, built-in AI

---

## Key Dimensions Discovered

During research, these additional dimensions emerged as important:

1. **Real-time sync** — WebSocket/subscription support for multi-agent collaboration
2. **CRDT support** — Conflict-free concurrent editing (AFFiNE)
3. **Vector search** — Hybrid semantic + graph queries (Cognee, ArangoDB, Neo4j)
4. **Temporal awareness** — Time-travel queries, bi-temporal data (Graphiti, TerminusDB)
5. **Self-hostability** — Data sovereignty for sensitive deployments
6. **License type** — MIT/Apache vs AGPL vs proprietary
7. **Rate limits** — Can significantly impact agent workflows (e.g., Coda's 10 POST/min)
