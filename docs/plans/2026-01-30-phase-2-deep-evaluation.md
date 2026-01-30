# Phase 2: Deep Evaluation Implementation Plan

> **For Claude:** This is a research plan, not a coding plan. Use parallel agents (5 at a time) to execute research tasks. Each agent fills out the Phase 2 template for one tool.

**Goal:** Complete technical deep-dive evaluation on all 23 tools that passed Phase 1 screening.

**Approach:** Dispatch research agents in batches of 5 to avoid rate limits. Each agent researches one tool against the Phase 2 template, writes results to per-tool file.

**Output:** Updated per-tool markdown files with Phase 2 data appended.

---

## Phase 2 Evaluation Template

Each tool must be evaluated against this template:

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url:
  auth_model: [api_key / oauth / other]
  rate_limits:
  webhooks: [yes / no]
  subscriptions: [yes / no]  # real-time updates
  mcp_server: [official / community / none]
  mcp_server_url:

data_model:
  node_structure: # description of how data is organized
  schema_flexibility: [fixed / dynamic / freeform]
  custom_fields: [yes / no]
  relations: # how entities link to each other

task_support:
  native_tasks: [yes / no]
  status_field: [yes / no]
  assignee_field: [yes / no]
  due_date: [yes / no]
  dependencies: [yes / no / workaround]
  dependency_description: # how dependencies work
  query_ready_tasks: [yes / no / how]  # can you query "tasks with no blockers"?

query_capabilities:
  simple_filters: [yes / no]
  graph_traversal: [yes / no]
  multi_hop_queries: # "find all tasks blocked by tasks owned by X"
  query_language: [none / datalog / graphql / cypher / sql / other]
  full_text_search: [yes / no]
  vector_search: [yes / no]

scale:
  documented_limits: # max nodes, storage, etc.
  concurrent_access: # multi-user/multi-agent support
  known_performance_issues:

hosting:
  hosted_option: [yes / no]
  hosted_pricing:
  self_host_complexity: [easy / moderate / complex / n/a]
  self_host_requirements: # Docker, K8s, etc.
  data_export: [yes / no / format]

real_time:
  sync_mechanism: # WebSocket, polling, CRDT, etc.
  latency: # if documented
  conflict_resolution: # how concurrent edits handled

agent_integration:
  mcp_tools_available: # list key tools/operations
  langchain_integration: [yes / no]
  llamaindex_integration: [yes / no]
  other_integrations:
```

---

## Tools to Evaluate (23 total)

### Batch 1: AI-Agent Native (4 tools)
1. Graphiti
2. Cognee
3. Knowledge Graph Studio
4. Beads

### Batch 2: PKM Tools Part 1 (5 tools)
5. Roam Research
6. Notion
7. AFFiNE
8. SiYuan
9. Trilium Notes

### Batch 3: PKM Tools Part 2 (4 tools)
10. Heptabase
11. Capacities
12. Reflect
13. Mem

### Batch 4: Task Management (3 tools)
14. Linear
15. Fibery
16. Coda

### Batch 5: Graph Databases Part 1 (4 tools)
17. Neo4j
18. Dgraph
19. ArangoDB
20. TerminusDB

### Batch 6: Graph Databases Part 2 (3 tools)
21. Atomic-Server
22. JanusGraph
23. LinkedDataHub

---

## Execution Instructions

### For Each Batch

1. **Dispatch 5 agents in parallel** (or fewer for smaller batches)
2. **Each agent prompt:**

```
You are researching **[TOOL NAME]** for Phase 2 deep evaluation.

## Your Task
Research this tool's technical capabilities and fill out the Phase 2 template.
Use web search to find API documentation, GitHub repos, and technical specs.

## Phase 2 Template
[INSERT FULL TEMPLATE FROM ABOVE]

## Research Focus Areas
1. **API docs** - Find official documentation, look for rate limits, auth model
2. **Data model** - How is data structured? Schema flexibility?
3. **Task support** - Can it model tasks with dependencies? How?
4. **Query capabilities** - What queries are possible? Graph traversal?
5. **Scale** - Any documented limits or performance notes?
6. **Real-time** - WebSocket? Subscriptions? How are conflicts handled?
7. **Agent integration** - MCP server details, LangChain/LlamaIndex support

## Output
APPEND the Phase 2 evaluation to the existing file:
/home/sami/swarm/docs/research/knowledge-graph-tools/per-tool/[tool-name].md

Add a "---" separator and "## Phase 2: Deep Evaluation" header before the YAML.
```

3. **Wait for batch to complete**
4. **Review results for gaps**
5. **Dispatch next batch**

---

## Post-Research Tasks

### Task A: Gap Analysis
After all batches complete, scan results for:
- Missing fields (agent couldn't find info)
- Inconsistent data (different agents interpreted differently)
- New dimensions discovered in `additional_sections`

### Task B: Normalize Data
Ensure all 23 tools have consistent:
- Yes/no answers (not "Yes" vs "yes" vs "true")
- Pricing format
- URL formats

### Task C: Update Comparison Matrix
Add Phase 2 columns to `docs/research/knowledge-graph-tools/comparison-matrix.md`:
- Real-time sync mechanism
- Query language
- Graph traversal capability
- Task/dependency support
- MCP server status

### Task D: Rank Candidates
Based on Phase 2 data, score tools against original requirements:
1. Task orchestration + knowledge storage
2. Real-time multi-agent collaboration
3. Dynamic schemas
4. Graph traversal queries
5. Scale to 100k+ nodes

---

## Deliverables

1. **Updated per-tool files** - Each of 23 files has Phase 2 section appended
2. **Updated comparison-matrix.md** - New columns from Phase 2 data
3. **top-candidates.md** - Ranked list with scoring rationale
4. **open-questions.md** - Items requiring hands-on testing

---

## Estimated Effort

- 6 batches × ~5 min per batch = ~30 min for research
- Gap analysis + normalization = ~15 min
- Matrix update + ranking = ~15 min
- **Total: ~1 hour**
