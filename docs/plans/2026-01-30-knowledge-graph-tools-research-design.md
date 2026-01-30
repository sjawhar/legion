# Knowledge Graph Tools Market Research Plan

## Objective

Produce a comparative analysis of knowledge graph tools to select a foundation for AI agent orchestration and knowledge storage.

## Context

We need a knowledge graph tool that serves as the central coordination layer for AI agent workflows — storing task state, knowledge artifacts, and dependencies that agents can read/write through APIs.

### Use Case Requirements

- **Primary purpose:** Both task orchestration AND knowledge accumulation equally
- **Interaction model:** Real-time collaborative (multiple agents working simultaneously)
- **Data structure:** Dynamic schemas with evolvable structure (like Tana supertags)
- **Scale:** Start medium (tens of thousands of nodes), plan for large (hundreds of thousands+)
- **Task support:** Data model must support task/project/dependency concepts; UI is nice-to-have
- **Querying:** Graph traversal essential, full query language is bonus

## Evaluation Criteria

### Must-Have (deal-breakers if missing)

- API access for programmatic read/write
- Web UI
- Graph traversal queries (multi-hop: "find all tasks blocked by X")
- Can represent: tasks, projects, dependencies, arbitrary knowledge nodes
- Real-time or near-real-time sync (agents see each other's changes)

### Strong Preferences

- Dynamic/evolvable schemas (add fields without migration)
- Open source
- Scalable to 100k+ nodes with concurrent access

### Nice-to-Have

- Mobile app
- Full query language (Datalog, GraphQL, Cypher)
- Built-in task UI (kanban, filters, etc.)
- Hosted option (avoid self-hosting)

## Evaluation Dimensions

1. **Basic Info** — pricing, platforms, open source status, company stability
2. **API Evaluation** — read/write, rate limits, webhooks, MCP server
3. **Data Model** — schema flexibility, node structure, custom fields
4. **Task Management Fit** — dependencies, status, "ready" queries
5. **Scale & Performance** — limits, concurrent access, known issues
6. **Hosting & Operations** — hosted option, self-host complexity, data export
7. **Maintenance & Health** — update frequency, roadmap, responsiveness
8. **User Reviews & Reputation** — Reddit, HN, Twitter sentiment
9. **AI Features** — built-in AI, LLM integrations, agent-friendly features

## Candidate Tools

### AI-Agent Native

- Graphiti (Zep) — real-time knowledge graphs for AI agents, open source, MCP server
- Cognee — graph + vector hybrid memory for AI
- Knowledge Graph Studio (WhyHow) — open source, API-first, designed for LLM/agent workflows
- Beads (Steve Yegge) — git-backed graph issue tracker for AI agents, open source

### PKM / Knowledge Graph Tools

- Roam Research
- Logseq
- Obsidian
- Tana (note: API currently write-only)
- Reflect
- Notion
- AFFiNE
- SiYuan
- Trilium Notes
- RemNote
- Anytype
- Capacities
- Heptabase
- Mem

### Task/Project Management

- Linear
- Fibery
- Coda

### Graph Databases (build-your-own foundation)

- Neo4j (+ Aura hosted)
- Dgraph
- TerminusDB
- ArangoDB
- Atomic-Server
- JanusGraph
- LinkedDataHub

## Execution Model

### Parallel Agent Architecture

```
Coordinator Agent
    │
    ├── Tool Agent: Roam Research
    ├── Tool Agent: Logseq
    ├── Tool Agent: Obsidian
    │   ... (one per tool, 27 total)
    └── Tool Agent: LinkedDataHub
```

### Coordinator Responsibilities

1. Dispatch all tool agents in parallel with standardized research template
2. Collect results, identify gaps in data parity
3. Redispatch targeted queries to fill gaps
4. Score/rank tools against criteria
5. Decide pass/fail for next phase
6. Maintain running comparison matrix

### Tool Agent Responsibilities

1. Research assigned tool only
2. Fill out standardized template for current phase
3. Add freeform notes and additional sections for anything template doesn't capture
4. Flag uncertainties
5. Return structured results to coordinator

### Gap-Filling Process

1. Coordinator scans `additional_notes` and `additional_sections` across all agents
2. If Agent A discovered something important (e.g., "has offline mode"), coordinator asks other agents the same question
3. Iterate until parity achieved on all relevant dimensions
4. Newly discovered dimensions get added to the comparison matrix

## Research Phases

### Phase 1: Initial Screening

For each tool, determine if it passes the basic requirements.

**Template:**
```yaml
tool_name:
official_url:
pricing: [free / freemium / paid_only / open_source]
pricing_details:
platforms:
  web_ui: [yes / no]
  desktop: [yes / no / which_os]
  mobile: [yes / no / ios / android / both]
api:
  exists: [yes / no]
  type: [REST / GraphQL / other]
  read_write: [read_only / write_only / both]
open_source: [yes / no]
repo_url:
last_commit:
github_stars:
screening_result: [PASS / FAIL]
fail_reason: # if applicable

additional_notes: |
  # Freeform observations

additional_sections:
  # Any new relevant dimensions discovered
```

**Pass criteria:** Has API + has web UI

**Output:** List of tools passing to Phase 2, elimination log

### Phase 2: Deep Evaluation

Full technical evaluation of passing tools.

**Template:**
```yaml
tool_name:
api_details:
  documentation_url:
  auth_model: [api_key / oauth / other]
  rate_limits:
  webhooks: [yes / no]
  subscriptions: [yes / no]
  mcp_server: [yes / no / community]
data_model:
  node_structure: # description
  schema_flexibility: [fixed / dynamic / freeform]
  custom_fields: [yes / no]
task_support:
  native_tasks: [yes / no]
  status_field: [yes / no]
  assignee_field: [yes / no]
  due_date: [yes / no]
  dependencies: [yes / no / workaround]
  query_ready_tasks: [yes / no / how]
query_capabilities:
  simple_filters: [yes / no]
  graph_traversal: [yes / no]
  query_language: [none / datalog / graphql / cypher / other]
scale:
  documented_limits:
  concurrent_access:
  known_issues:
hosting:
  hosted_option: [yes / no]
  self_host_complexity: [easy / moderate / complex / n/a]
  data_export: [yes / no / format]

additional_notes: |

additional_sections:
```

### Phase 3: Community & Sentiment Research

Gather user feedback and reputation data.

**Template:**
```yaml
tool_name:
reddit:
  search_terms_used:
  overall_sentiment: [positive / mixed / negative]
  common_praise: []
  common_complaints: []
  notable_threads: []
hackernews:
  overall_sentiment: [positive / mixed / negative]
  notable_discussions: []
twitter:
  overall_sentiment: [positive / mixed / negative]
  notable_takes: []
comparison_mentions:
  - from:
    to:
    reason:

additional_notes: |

additional_sections:
```

### Phase 4: AI Features Audit

Evaluate AI and agent-specific capabilities.

**Template:**
```yaml
tool_name:
builtin_ai:
  has_ai_features: [yes / no]
  features: []
llm_integrations:
  openai: [yes / no]
  claude: [yes / no]
  local_models: [yes / no]
  other: []
agent_friendly:
  mcp_server: [yes / no / community]
  structured_output: [yes / no]
  context_management:
  agent_specific_docs: [yes / no]

additional_notes: |

additional_sections:
```

### Phase 5: Synthesis

Coordinator consolidates all research into final deliverables. No tool agents needed.

## Deliverables

### Primary: Comparison Matrix

- Rows: All tools that passed Phase 1 screening
- Columns: All 9 evaluation dimensions + discovered dimensions
- Cells: Standardized values where possible, notes where needed
- Color coding: Green (strong), Yellow (acceptable), Red (weak/missing)

### Secondary Deliverables

1. **Elimination Log** — tools that failed screening and why
2. **Per-Tool Deep Dives** — full completed templates for each tool
3. **Sentiment Summary** — aggregated community feedback patterns
4. **Top Candidates Analysis** — 3-5 recommended tools with trade-off analysis
5. **Open Questions** — things requiring hands-on testing or vendor contact

### Output Location

```
docs/research/knowledge-graph-tools/
├── comparison-matrix.md
├── elimination-log.md
├── sentiment-summary.md
├── top-candidates.md
├── open-questions.md
└── per-tool/
    ├── roam-research.md
    ├── logseq.md
    ├── notion.md
    └── ...
```

## Sources

Research conducted using:
- [Graphiti GitHub](https://github.com/getzep/graphiti)
- [Tana Input API Docs](https://tana.inc/docs/input-api)
- [Beads GitHub](https://github.com/steveyegge/beads)
- [Knowledge Graph Studio announcement](https://medium.com/enterprise-rag/open-sourcing-the-whyhow-knowledge-graph-studio-powered-by-nosql-edce283fb341)
- [Neo4j Knowledge Graph](https://neo4j.com/use-cases/knowledge-graph/)
- [LinkedDataHub](https://atomgraph.github.io/LinkedDataHub/)
- Various Roam Research alternative comparison articles
