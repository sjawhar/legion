# Beads (by Steve Yegge)

```yaml
tool_name: Beads
category: AI-Agent Native
official_url: https://github.com/steveyegge/beads
pricing: open_source
pricing_details: |
  Completely free and open source under MIT license. No external service,
  no API keys, no server management required. Runs locally using Git for
  synchronization - zero cost infrastructure.
platforms:
  web_ui: yes  # Community-built web UIs available (beads-ui, beads-dashboard, beads-kanban-ui, etc.) plus official Monitor WebUI example
  desktop: yes  # macOS/Linux/Windows/FreeBSD - CLI tool with native apps like Beadster (macOS) and Parade (Electron)
  mobile: no
api:
  exists: yes
  type: other  # CLI with --json flag, MCP server (beads-mcp), RPC daemon
  read_write: both
open_source: yes
repo_url: https://github.com/steveyegge/beads
last_commit: 2026-01-26  # v0.49.1 released
github_stars: 13700  # 13.7k
screening_result: PASS
fail_reason:

additional_notes: |
  ## What Beads Is
  Beads is a distributed, git-backed graph issue tracker designed specifically for AI coding
  agents. Created by Steve Yegge (former Google/Amazon engineer), it solves context window
  limitations by providing persistent, structured memory for coding agents.

  Described as "a magical 4-dimensional graph-based git-backed fairy-dusted issue-tracker
  database, designed to let coding agents track all your work and never get lost again."

  ## Architecture
  - Issues stored as JSONL in `.beads/` directory
  - SQLite local cache with background daemon for auto-sync
  - Git as distributed database - versioned, branched, merged like code
  - Hash-based IDs (e.g., bd-a1b2) prevent merge collisions in multi-agent workflows
  - Directed acyclic graph (DAG) with explicit dependencies and priority levels

  ## Agent-First Design
  - JSON output on all commands (`--json` flag)
  - Dependency tracking with four different link types (including provenance)
  - Auto-ready task detection via `bd ready` command
  - Hierarchical task organization (epics/tasks/subtasks)
  - Semantic compaction for context optimization
  - Built for "execution" not just "planning"

  ## MCP Server Support
  Official MCP server available via PyPI: `pip install beads-mcp`
  - 9 tools: beads_create, beads_list, beads_show, beads_update, beads_close,
    beads_ready, beads_sync, beads_dep_add, beads_dep_tree
  - Multi-workspace support with per-request routing
  - Aggressive context reduction features
  - Primary use: MCP-only environments like Claude Desktop
  - CLI recommended over MCP for shell environments (1-2k vs 10-50k tokens)

  ## Web UI Ecosystem
  Official:
  - Monitor WebUI (official example) - real-time issue tracking dashboard

  Community-built (documented in COMMUNITY_TOOLS.md):
  - beads-ui - local web interface with live updates and kanban
  - beads-dashboard - metrics dashboard with lead time/throughput insights
  - beads-kanban-ui - visual kanban with git branch tracking
  - beads-pm-ui - Gantt chart and dependency visualization
  - beads-viz-prototype - interactive HTML visualization

  ## Additional Integrations
  - VS Code extensions (vscode-beads, ANAL Beads, Beads-Kanban)
  - Neovim plugin (nvim-beads)
  - Emacs interface (beads.el)
  - Multiple terminal UIs (beads_viewer, bdui, perles, lazybeads)
  - Claude Code orchestration skill (beads-orchestration)
  - Jira sync (jira-beads-sync)

  ## Installation
  - npm: `npm install -g @beads/bd`
  - Homebrew: `brew install beads`
  - Go: `go install github.com/steveyegge/beads/cmd/bd@latest`

  ## Development Context
  - 130k lines of Go code (approx half tests)
  - 100% "vibe coded" in 6 days with Claude
  - Alpha status - core features work but API may change before 1.0
  - 5,603 total commits, very active development
  - Tens of thousands of users in daily workflows

additional_sections:
  multi_agent_support:
    description: |
      Explicitly designed for multi-agent and multi-branch workflows. Hash-based IDs
      prevent merge collisions. Four dependency link types including "provenance" for
      forensics when multiple workers create issues. Contributor vs maintainer role
      detection. Stealth mode for personal use.

  context_window_optimization:
    description: |
      Core value proposition is solving context window limitations. Agents can track
      long-horizon tasks without losing context between sessions. MCP server includes
      aggressive context reduction with parameters like brief, brief_deps, fields,
      and max_description_length.

  not_a_knowledge_graph:
    description: |
      Important clarification: Beads is an issue/task tracker with graph-like dependencies,
      NOT a general-purpose knowledge graph. It tracks work items (issues, epics, tasks)
      with dependency links rather than arbitrary knowledge entities. Categorized here
      due to its graph-based structure and agent-native design, but serves a different
      purpose than tools like Neo4j or knowledge bases.
```

## Sources
- [GitHub Repository](https://github.com/steveyegge/beads)
- [Introducing Beads - Medium](https://steve-yegge.medium.com/introducing-beads-a-coding-agent-memory-system-637d7d92514a)
- [The Beads Revolution - Medium](https://steve-yegge.medium.com/the-beads-revolution-how-i-built-the-todo-system-that-ai-agents-actually-want-to-use-228a5f9be2a9)
- [Beads Best Practices - Medium](https://steve-yegge.medium.com/beads-best-practices-2db636b9760c)
- [MCP Server Documentation](https://steveyegge.github.io/beads/integrations/mcp-server)
- [Community Tools](https://github.com/steveyegge/beads/blob/main/docs/COMMUNITY_TOOLS.md)
- [beads_viewer (Terminal UI)](https://github.com/Dicklesworthstone/beads_viewer)
- [beads-viewer (Web UI)](https://github.com/mgalpert/beads-viewer)

---

## Phase 2: Deep Evaluation

```yaml
api_details:
  documentation_url: https://steveyegge.github.io/beads/
  auth_model: none  # Local tool, no authentication required
  rate_limits: none  # Local CLI tool with no API rate limits (unlike GitHub Issues)
  webhooks: no
  subscriptions: no  # File system watching for local changes, not traditional subscriptions
  mcp_server: official
  mcp_server_url: https://pypi.org/project/beads-mcp/

data_model:
  node_structure: |
    Issues are the primary entity with ~40 fields organized into:
    - Core fields: ID (hash-based, e.g., bd-a1b2), title, description, design, acceptance_criteria
    - Workflow metadata: status (open/in_progress/closed), priority (0-4), type (bug/feature/task/epic/chore)
    - Assignment: assignee field
    - Relationships: dependencies (via separate table), labels, comments
    - Timestamps: created, updated, closed, deleted
    - Parent-child hierarchy: epics contain tasks which can have subtasks
    Dual storage: SQLite (beads.db) for fast queries, JSONL (issues.jsonl) for git versioning.
  schema_flexibility: fixed  # Predefined ~40 fields, no user-defined custom fields
  custom_fields: no
  relations: |
    19 dependency types organized into categories:
    - Workflow (affect ready-work): blocks, parent-child, conditional-blocks, waits-for
    - Association: related, discovered-from (provenance tracking)
    - Graph: replies-to, relates-to, duplicates, supersedes
    - HOP Foundation: authored-by, assigned-to, approved-by, attests
    - Cross-project: tracks
    - Reference: until, caused-by, validates, delegated-from
    Only 4 types affect ready-work calculation: blocks, parent-child, conditional-blocks, waits-for.

task_support:
  native_tasks: yes  # Core purpose is task/issue tracking
  status_field: yes  # open, in_progress, closed
  assignee_field: yes
  due_date: no  # Not explicitly documented as a core field
  dependencies: yes
  dependency_description: |
    First-class dependency support with 19 dependency types. Key blocking types:
    - blocks: Hard dependency, blocker must complete first
    - parent-child: Hierarchical (epic->task->subtask)
    - conditional-blocks: Conditional dependencies
    - waits-for: Soft wait dependency
    Dependencies stored in separate SQLite table with issue_id, depends_on_id, type.
    Cycle detection via recursive CTE prevents circular dependencies.
  query_ready_tasks: yes  # `bd ready` command - core feature

query_capabilities:
  simple_filters: yes  # Filter by status, priority, type, assignee, labels
  graph_traversal: yes  # Dependency tree traversal via `bd dep tree`, recursive CTE queries
  multi_hop_queries: |
    Limited. Can traverse dependency graphs but no arbitrary graph query language.
    `bd dep tree <id>` shows full dependency hierarchy.
    `bd ready` finds all tasks with no blocking dependencies (single-hop check).
    No support for complex queries like "tasks blocked by tasks owned by X".
  query_language: none  # CLI commands with flags, no query DSL
  full_text_search: yes  # --title-contains, --desc-contains flags
  vector_search: no

scale:
  documented_limits: |
    - Performance degrades noticeably beyond 200 active tasks
    - JSONL file limit ~25k tokens (~500 issues) for agents reading whole file
    - For 100k+ issues, recommended to filter exports or use multiple databases
    - Hash-based IDs use 4-8 characters adaptive to database size
    - Ready-work query: 25x speedup with materialized view (~29ms vs 752ms)
  concurrent_access: |
    Multi-agent support is a core design goal:
    - Hash-based IDs prevent merge collisions across agents/branches
    - Exclusive file lock (.beads/.sync.lock) prevents concurrent sync
    - 17+ concurrent agents caused version check timeouts in practice
    - Steve Yegge ran 12 concurrent agents for 2 weeks but found it unsustainable
    - Recommended: 1-3 concurrent agents with frequent session restarts
  known_performance_issues: |
    - bd version check causes contention under high concurrency (17+ agents)
    - Large issues.jsonl files (>25k tokens) problematic for agent file reading
    - Daemon requires Unix socket, can be problematic in CI/CD environments

hosting:
  hosted_option: no  # Local-only tool
  hosted_pricing: n/a
  self_host_complexity: easy  # Single binary CLI, no server infrastructure
  self_host_requirements: |
    - CLI binary (Go): npm, Homebrew, or go install
    - Git repository for sync
    - SQLite (bundled)
    - Optional: Background daemon for auto-sync (Unix socket)
    - No Docker/K8s required
  data_export: yes  # JSONL format in .beads/issues.jsonl, auto-synced with git

real_time:
  sync_mechanism: |
    Git-based synchronization with daemon:
    - Background daemon auto-syncs SQLite <-> JSONL with 5-second debounce
    - File system watching detects external JSONL changes
    - Pull-before-export pattern prevents data loss
    - Sync-branch mode isolates beads metadata in dedicated git branch
    - Manual sync via `bd sync` when daemon disabled
  latency: |
    - 5-second debounce on export after operations
    - 500ms flush window for batched operations
    - Local SQLite queries: ~29ms for ready-work with materialization
  conflict_resolution: |
    3-way merge following Git's algorithm:
    - Scalars (title, status): Last-Write-Wins (LWW) based on timestamp
    - Labels/Dependencies: Union merge (preserves both sets)
    - Comments: Append-only, chronologically sorted, deduplicated by ID
    - No silent data loss: If one side modifies while other deletes, modification wins
    - Custom merge driver installed via `bd init`
    - Manual conflict resolution with reimport as fallback
    - 24-hour clock skew warning threshold

agent_integration:
  mcp_tools_available: |
    12 tools via beads-mcp (official):
    - init: Initialize bd in directory
    - create: Create issue (bug/feature/task/epic/chore)
    - list: List issues with filters (status, priority, type, assignee)
    - ready: Find unblocked tasks ready to work on
    - show: Show detailed issue info with dependencies
    - update: Update issue properties
    - close: Close completed issue
    - dep: Add dependency relationships
    - blocked: Get blocked issues
    - stats: Get project statistics
    - reopen: Reopen closed issue
    - set_context: Set default workspace for subsequent calls
    All tools support optional workspace_root for multi-project routing.
  langchain_integration: no  # No documented integration
  llamaindex_integration: no  # No documented integration
  other_integrations: |
    - Claude Code plugin with slash commands (/beads:*)
    - VS Code extensions (vscode-beads, ANAL Beads, Beads-Kanban)
    - Neovim plugin (nvim-beads)
    - Emacs interface (beads.el)
    - Terminal UIs (beads_viewer, bdui, perles, lazybeads)
    - OpenCode plugin (36 tools)
    - Jira sync (jira-beads-sync)
    - Community web UIs (beads-ui, beads-dashboard, beads-kanban-ui)
    - CLI with --json flag for programmatic access
    - Unix socket RPC daemon for direct integration
```

## Phase 2 Sources
- [Official Documentation](https://steveyegge.github.io/beads/)
- [beads-mcp PyPI](https://pypi.org/project/beads-mcp/)
- [DeepWiki Technical Analysis](https://deepwiki.com/steveyegge/beads)
- [Better Stack Guide](https://betterstack.com/community/guides/ai/beads-issue-tracker-ai-agents/)
- [FAQ Documentation](https://steveyegge.github.io/beads/reference/faq)
- [SYNC.md](https://github.com/steveyegge/beads/blob/main/docs/SYNC.md)
- [PLUGIN.md](https://github.com/steveyegge/beads/blob/main/docs/PLUGIN.md)
