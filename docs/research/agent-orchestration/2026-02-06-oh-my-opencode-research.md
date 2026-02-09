# Oh My OpenCode Research

**Date:** 2026-02-06
**Agent:** Oh My OpenCode deep dive researcher
**Focus:** Architecture, agent hierarchy, orchestration patterns, cross-model workflows

---

## What Is Oh My OpenCode?

**Oh My OpenCode** is a batteries-included orchestration layer and plugin for [OpenCode](https://opencode.ai). Created by [Yeon Gyu Kim (code-yeongyu)](https://github.com/code-yeongyu/oh-my-opencode), it transforms OpenCode from a single-agent tool into a production-ready multi-agent development harness.

Think of it as "Ubuntu to Debian" — taking a powerful but complex base (OpenCode) and adding curated defaults, specialized agents, hooks, and workflows.

**Repository:** https://github.com/code-yeongyu/oh-my-opencode (5.3K+ stars)
**Official Site:** https://ohmyopencode.com

---

## Agent Architecture

### Primary Agents

| Agent | Model | Purpose |
|-------|-------|---------|
| **Sisyphus** | Claude Opus 4.6 | Main orchestrator, fallback coordinator |
| **Atlas** | Claude Sonnet 4.5 | Master orchestrator for complex operations |
| **Hephaestus** | GPT-5.3 Codex | Autonomous deep worker for substantial engineering |
| **Oracle** | GPT-5.2 | Architecture consultation and debugging |
| **Librarian** | GLM-4.7 | Documentation search, remote codebase retrieval |
| **Explore** | Xai Grok Code Fast-1 | Fast codebase grep and exploration |
| **Multimodal-Looker** | Gemini-3 Flash | PDF and image analysis |
| **Prometheus** | Claude Opus 4.6 | Strategic planning and task decomposition |
| **Metis** | Claude Opus 4.6 | Pre-planning gap analysis (temp: 0.3) |
| **Momus** | GPT-5.2 | Plan validation (temp: 0.1) |
| **Sisyphus-Junior** | Claude Sonnet 4.5 | Category-spawned executor tasks (temp: 0.1) |

Each agent has **fallback chains** (e.g., Sisyphus → Kimi K2.5 → GLM-4.7 → GPT-5.3 Codex → Gemini-3 Pro).

---

## Multi-Agent Coordination: The Prometheus → Atlas → Junior Workflow

### Layer 1: Planning (Prometheus + Metis + Momus)

**Prometheus** (Planner):
- Conducts intelligent interviews rather than simple planning
- Adapts approach based on task intent (refactoring = safety, new builds = pattern discovery)
- Researches with helper agents
- Creates detailed plans stored in `.sisyphus/plans/*.md`

**Metis** (Pre-Planning Consultant):
- Analyzes gaps, edge cases, implicit assumptions
- Ensures Prometheus hasn't missed critical details

**Momus** (Plan Reviewer):
- Validates plans against four criteria:
  - **Clarity**: Does each task specify WHERE to find implementation details?
  - **Verification**: Are acceptance criteria concrete and measurable?
  - **Context**: Is there sufficient context (<10% guesswork)?
  - **Big Picture**: Is purpose/background/workflow clear?
- Plans loop back to Prometheus until approved

**User Workflow:**
1. Press `Tab` to enter Prometheus mode
2. Describe work → Prometheus interviews you
3. Review plan files in `.sisyphus/plans/`
4. Run `/start-work` to begin execution

### Layer 2: Execution (Atlas - The Orchestrator)

Atlas functions as a conductor that doesn't play instruments. It:
- Reads the approved plan
- Identifies parallelizable task groups
- Builds detailed 7-section prompts for subagents:
  1. Task description
  2. Desired outcome
  3. Skills to load
  4. Tools to use
  5. Must-do requirements
  6. Constraints
  7. Context
- **Accumulates wisdom** from each task (patterns, conventions, failures)
- Forwards learnings to subsequent tasks
- **Verifies results independently — "NEVER trust subagent claims"**

**State Tracking:** `boulder.json` tracks current plan and session ID. Work continues across session interruptions.

### Layer 3: Workers (Specialized Agents)

**Sisyphus-Junior** (Primary Executor):
- Executes individual tasks with disciplined focus
- Cannot delegate (blocked from task tool)
- Obsessively tracks todos
- Must pass language server diagnostics before completion
- System reminders: "You have incomplete todos! Complete ALL before responding."

**Domain-Specific Workers:**
- Oracle: Backend architecture and complex logic
- Explore: Codebase search
- Librarian: Documentation lookup
- Frontend Engineer (Gemini 3 Pro): UI/UX work

---

## Task Delegation: Categories + Skills

Rather than specifying model names (creates bias), the system uses **semantic categories**:

- `visual-engineering`: Frontend/UI design work
- `ultrabrain`: Strategic reasoning and complex architecture
- `quick`: Trivial single-file changes
- `artistry`: Creative/novel problem-solving

**Skills** add domain expertise through prompt prefixes. A task might combine:
```
category="visual-engineering"
load_skills=["frontend-ui-ux"]
```

Models matched by purpose, not brand name.

---

## Cross-Model Workflow Support

**Providers used:**
- OpenAI: GPT 5.2 (debugging), GPT 5.3 Codex (deep work)
- Anthropic: Claude Opus 4.5 (main), Sonnet 4.5 (docs), Haiku 4.5 (fast grep)
- Google: Gemini 3 Pro (frontend specialization)
- Others: Kimi K2.5, GLM-4.7, Xai Grok Code Fast-1

**Configuration Options:**
- Per-model temperature overrides
- Permission controls
- Concurrency limits per provider
- Multi-account load balancing
- Endpoint fallback
- Automatic token refresh

**Example Workflow:**
> "While Gemini 3 Pro writes the frontend as a background task, Claude Opus 4.5 handles the backend. Stuck debugging? Call GPT 5.2 for help. When the frontend reports done, verify and ship."

---

## Key Features

### LSP & AST Integration

- **LSP Tools**: Type information, symbol navigation, diagnostics, refactoring
- **AST-grep**: Structural code search/replacement for 25+ languages
- **Deterministic Refactoring**: Rename symbols, extract functions, code actions
- **IDE-like Capabilities**: Jump to definitions, find references across workspace

### Productivity Hooks

**Ralph Loop:**
- Iterative feedback loop — agent improves work until all tests pass
- `/ralph-loop 'Build a REST API with authentication'`

**Todo Continuation Enforcer:**
- Forces agent to continue if it quits mid-task
- Monitors todo items and prompts for continuation

**Comment Checker:**
- Prevents AI from adding excessive comments
- Keeps generated code indistinguishable from human-written

### Built-in MCPs
- **Exa**: Web search with URL analysis
- **Context7**: Official documentation lookup
- **Grep.app**: GitHub code search

### "Ultrawork" Activation
Simply include `ultrawork` (or `ulw`) in prompts to activate parallel agents, background tasks, deep exploration, relentless execution.

### Background Task Management
- True parallel agent execution
- Per-provider concurrency limits
- Independent queues and counters per model
- Slot-based execution (blocks if limit reached)
- Polling every 2 seconds for completion detection

### Forced Completion
Tasks don't terminate mid-execution. If an agent quits:
- Hooks trigger reinitiation with modified context
- System forces task completion
- "Boulder-pushing" metaphor (Sisyphus) reflects relentless progress

---

## Comparison with Legion

### Similarities

1. **Multi-Agent Orchestration**: Both use specialized agents for different phases
2. **State Tracking**: `boulder.json` ↔ Legion's issue state machine
3. **Persistent Execution**: Sessions survive interruptions
4. **Planning-First Workflow**: Prometheus interview → plan → execute ↔ architect/plan/implement
5. **Task Decomposition**: Complex tasks broken into sub-tasks
6. **Cross-Model Support**: Different models for different capabilities (OMOC); planned for Legion

### Differences

| Oh My OpenCode | Legion |
|----------------|--------|
| Single codebase, terminal-based | Multiple jj workspaces |
| Plans in `.sisyphus/plans/` | Plans posted to Linear issue |
| Atlas delegates in-session | Controller daemon dispatches workers |
| Background tasks for parallelism | Parallel workers in separate workspaces |
| State in `boulder.json` | State in Linear status + labels |
| Review via hooks/validation | Review via GitHub PR + Linear status |

### What Legion Does Better

1. **True Isolation**: jj workspaces prevent cross-contamination
2. **Issue Tracking Integration**: Linear as source of truth
3. **Long-Running Daemon**: Controller manages multiple workers
4. **PR-Based Review**: Explicit GitHub PR workflow
5. **Retro Phase**: Explicit learning documentation

### What Oh My OpenCode Does Better

1. **Background Parallelism**: Within-session parallel tasks (fine-grained)
2. **LSP/AST Integration**: Deep IDE-like capabilities
3. **Cross-Model Flexibility**: Mix models by task
4. **Hooks Ecosystem**: 25+ pre-built productivity hooks
5. **Community & Maturity**: 5.3K stars, active ecosystem

---

## Patterns Worth Adopting in Legion

### Planning Triad (Prometheus + Metis + Momus)
- Could enhance Legion's architect mode with Metis-like gap analysis
- Add explicit plan validation criteria before Todo

### Wisdom Accumulation
- Atlas forwards learnings from task to task
- Legion workers could accumulate patterns in Linear comments
- Share learnings across workers via controller

### Categories Over Models
- Semantic categories (`visual-engineering`, `ultrabrain`) avoid model bias
- Legion could use issue labels/complexity for worker assignment

### Forced Completion
- Todo Enforcer prevents incomplete work
- Legion's stuck detection kills workers — consider persistence reminders first

### Background Agents for Exploration
- Parallel lightweight agents map terrain cheaply
- Use faster/cheaper models for codebase exploration

### LSP Integration
- Deterministic refactoring prevents hallucinations
- Legion should leverage LSP heavily in implement mode

---

## Ecosystem

### Oh My OpenCode Slim
https://github.com/alvinunreal/oh-my-opencode-slim — Lightweight fork: reduced tokens, core orchestration only.

### Related Projects
- **swarm-tools** (joelhooks): 40+ tools for multi-agent coordination
- **oh-my-claudecode** (Yeachan-Heo): Multi-agent for Claude Code (5 execution modes)
- **OpenAgentsControl** (darrenhinde): Plan-first workflows with approval-based execution
- **Awesome OpenCode**: https://github.com/awesome-opencode/awesome-opencode

---

## Sources

- [Oh My OpenCode GitHub](https://github.com/code-yeongyu/oh-my-opencode)
- [Oh My OpenCode Website](https://ohmyopencode.com/)
- [AGENTS.md](https://github.com/code-yeongyu/oh-my-opencode/blob/dev/AGENTS.md)
- [Orchestration Guide](https://github.com/code-yeongyu/oh-my-opencode/blob/dev/docs/orchestration-guide.md)
- [Understanding Orchestration System](https://github.com/code-yeongyu/oh-my-opencode/blob/dev/docs/guide/understanding-orchestration-system.md)
- [Multi-Model Deep Dive (Medium)](https://thamizhelango.medium.com/boosting-ai-coding-productivity-with-multi-model-agents-a-deep-dive-into-oh-my-opencode-25ebaf0e8d6b)
- [Best Practices Guide](https://opencodeguide.com/en/oh-my-opencode-best-practices)
- [OpenCode vs Claude Code](https://www.builder.io/blog/opencode-vs-claude-code)
- [DeepWiki: Sisyphus Orchestrator](https://deepwiki.com/code-yeongyu/oh-my-opencode/4.1-sisyphus-orchestrator)
- [DeepWiki: Task Execution](https://deepwiki.com/code-yeongyu/oh-my-opencode/6.2-task-execution-and-polling)
