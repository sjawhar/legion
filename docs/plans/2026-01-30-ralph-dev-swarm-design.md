# Ralph Dev Swarm: Autonomous Development System

A system for autonomous, parallel software development using Claude Code agents coordinated through Linear and jj.

## Overview

Ralph Dev Swarm combines:
- **Structured planning** from superpowers plugin (write plan → execute plan)
- **Ralph loop** pattern (iterate until done with fresh context each iteration)
- **Parallel execution** via independent workers in tmux windows and jj workspaces

The system enables long-running autonomous development sessions where multiple agents work in parallel on independent tasks, coordinated through Linear as the source of truth.

## Architecture

```
                         Daemon (persistent process)
                              │
              ┌───────────────┼───────────────┐
              │               │               │
              ▼               ▼               ▼
         Start            Health loop    Spawn Supervisor
         Controller       (every 3 min)  (if heartbeat stale)
              │
              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    tmux session: ralph-<project>                 │
├───────────┬──────────┬──────────┬───────────────────────────────┤
│ controller│ ENG-21   │ ENG-22   │ ENG-30                        │
│           │ (execute)│ (execute)│ (plan)                        │
└───────────┴──────────┴──────────┴───────────────────────────────┘
      │            │          │           │
      │            └──────────┴───────────┘
      │                       │
      │              jj workspaces (code isolation)
      │
      └──► Dispatches workers, merges results
                              │
                              ▼
                    ┌──────────────────┐
                    │      Linear      │
                    │  (source of truth)│
                    └──────────────────┘
```

### Components

| Component | Role | Lifecycle |
|-----------|------|-----------|
| **Daemon** | Entry point, health loop, spawns supervisor | Persistent process |
| **Supervisor** | Reasons about health, restarts/nudges components | Ephemeral (spawned when needed) |
| **Controller** | Polls Linear, routes tasks, dispatches workers, merges | Ephemeral (Ralph loop) |
| **Workers** | Execute assigned skill (execute, plan, review, etc.) | Ephemeral, may iterate on task |

## State Management

### Linear (Task State)

```
Linear Project: "Project Name"
│
├─► Phase Issue: "Research"
│   ├── Sub-issue: Research competitor A
│   ├── Sub-issue: Research competitor B (blocked by: none)
│   └── Sub-issue: Document findings (blocked by: above)
│
├─► Phase Issue: "Spec: Data Model" (blocked by: Research)
│   ├── Sub-issue: Define schema
│   └── Sub-issue: Define ID generation
│
└─► Phase Issue: "Implement: Core" (blocked by: Spec)
    ├── Sub-issue: Set up workspace
    └── Sub-issue: Create command (blocked by: above)
```

**Linear provides:**
- Issue hierarchy (phases → tasks)
- Dependencies (blocked by / blocking)
- Status tracking (Todo → In Progress → Done)
- Comments for async communication
- Labels for signaling

### jj (Code State)

- **Working copy description**: `ENG-21: Set up Rust workspace`
- **Workspaces**: One per code worker, named by issue ID
- **Commits**: Close issues via PR workflow

### tmux (Process State)

- **Session**: One per Linear project
- **Windows**: One per active worker, named by issue ID
- **Supervisor + Controller**: Dedicated windows

### Session Files

Claude session files land in: `~/.claude/projects/-path-to-workspace/`

The issue ID ties everything together:
- tmux window: `ENG-21`
- jj workspace: `~/project/ENG-21/`
- Session files: `~/.claude/projects/-home-...-ENG-21/*.jsonl`

## Communication

### Labels

| Label | Meaning | Set by |
|-------|---------|--------|
| `user-input-needed` | Agent blocked waiting for user | Agent |
| `user-feedback-given` | User proactively provided direction | User |
| `agent-feedback-given` | Agent provided update/findings (FYI) | Agent |
| `agent-input-needed` | Agent needs input from another agent | Agent |

### Communication Channels

| Channel | Use Case |
|---------|----------|
| Linear comments | Async communication (questions, updates, context) |
| Linear status | Task progress (Todo → In Progress → Done) |
| Linear labels | State signaling (see above) |
| jj commits | Code handoff, progress log |
| GitHub PRs | Code review, user feedback opportunity |
| tmux send-keys | Controller → Worker intervention (rare) |
| tmux capture-pane | Controller monitoring worker output |

### Polling Linear

Controller polls Linear for updates (~1 min intervals):

```graphql
query PollUpdates($since: DateTime!, $projectId: ID!) {
  issues(
    filter: {
      project: { id: { eq: $projectId } }
      updatedAt: { gte: $since }
    }
    orderBy: updatedAt
    first: 100
  ) {
    nodes {
      id identifier state { name }
      labels { nodes { name } }
      comments(first: 5, orderBy: createdAt) {
        nodes { body createdAt user { displayName } }
      }
    }
  }
}
```

Rate limits are generous: 5,000 req/hr, 250,000 complexity points/hr.

## Workflow

### Controller Logic (Each Iteration)

```
1. Poll Linear for state + check labels
   └─► Process `user-feedback-given` labels
   └─► Process `agent-input-needed` labels

2. Check tmux for running workers
   └─► Which windows exist?
   └─► Capture-pane to check if stuck

3. Identify ALL dispatchable work:
   ┌─────────────────────────────────────────────────────────┐
   │  EXECUTE pool: Unblocked tasks ready to implement       │
   │  PLAN pool: Phases that need breakdown into tasks       │
   │  GROOM pool: Roadmap gaps, reprioritization needs       │
   │  RESEARCH pool: Open questions needing investigation    │
   └─────────────────────────────────────────────────────────┘

4. Dispatch ALL pools in parallel:
   - Create jj workspaces for code workers
   - Create tmux windows named by issue ID
   - Start Claude in each window

5. Monitor running workers (optional intervention)

6. Merge completed workspaces
   └─► Invoke conflict resolution if needed

7. Exit (Ralph loop restarts with fresh context)
```

### Worker Lifecycle

**Self-terminating workers**: Workers clean up after themselves when done, avoiding resource leaks. The controller spawns workers but doesn't manage their shutdown.

```bash
# Controller spawns worker
ISSUE_ID="ENG-21"

# Create jj workspace for code work
jj workspace add "$ISSUE_ID"

# Create tmux window
tmux new-window -t "$PROJECT_SESSION" -n "$ISSUE_ID" -d

# Start Claude (session file auto-created based on workspace path)
tmux send-keys -t "$PROJECT_SESSION:$ISSUE_ID" "cd ~/project/$ISSUE_ID && claude" Enter

# Send initial prompt
tmux send-keys -t "$PROJECT_SESSION:$ISSUE_ID" "Use ralph-dev-execute skill for $ISSUE_ID" Enter
tmux send-keys -t "$PROJECT_SESSION:$ISSUE_ID" Enter
```

**Worker self-termination** (triggered by Claude Code hooks or skill logic):

```bash
# Worker completes task, then cleans up
linear issue update "$ISSUE_ID" --status "Done"
jj workspace forget "$ISSUE_ID"
tmux kill-window -t "$SESSION:$ISSUE_ID"
```

### PR Workflow

1. Worker completes implementation
2. Worker creates PR referencing Linear issue
3. PR can merge without blocking on user review
4. User reviews async, provides feedback via PR comments or Linear
5. Worker picks up feedback in subsequent iterations if needed

## Autonomy Principles

### Decision Hierarchy (How Workers Resolve Uncertainty)

1. **Research** - WebSearch, read docs, look at similar projects
2. **Consult subagents** - code-architect, ux-designer, red-teamer
3. **Spike implementations** - Build options, benchmark, compare empirically
4. **Make a decision** - Pick one, document rationale, move on
5. **Non-blocking question** - Post comment, add `agent-feedback-given`, continue other work
6. **Blocking (last resort)** - Add `user-input-needed`, only when NO other work can proceed

### Key Principle

> The user should only be consulted when absolutely necessary. Agents do research, consult other agents, run experiments, and red-team their own ideas before asking the user.

## Parallelization

### What Can Run in Parallel

| Activity | Needs jj workspace? | Can parallelize with |
|----------|---------------------|----------------------|
| Execute (code) | Yes | Other executes, plan, groom, research |
| Plan | No | Execute, groom, research |
| Groom | No | Execute, plan, research |
| Research | No | Everything |

### Constraints

- Code workers need separate jj workspaces
- Same Linear issue shouldn't have concurrent workers
- True dependencies must be respected (blocked-by)

### Merge Strategy

When workers complete:
1. Controller detects completion (worker window closed or status changed)
2. Merge jj workspaces: `jj new worker1@ worker2@ @`
3. Resolve conflicts if needed (invoke conflict resolution skill)
4. Clean up workspaces: `jj workspace forget $ISSUE_ID`

### Monitoring with Self-Terminating Workers

Self-termination simplifies resource management but requires correlating Linear state with tmux state:

| Condition | Detection | Action |
|-----------|-----------|--------|
| **Stuck worker** | tmux window exists, capture-pane shows idle | Restart or nudge |
| **Orphaned issue** | Linear "In Progress" but no tmux window | Respawn worker |
| **Zombie window** | tmux window exists but Linear shows "Done" | Kill window |
| **Healthy worker** | tmux window active, Linear "In Progress" | No action |

Linear remains the source of truth. The controller/supervisor correlates:
```bash
# Get In Progress issues
linear issue list --project "$PROJECT" --status "In Progress" --json | jq -r '.[].identifier'

# Get active tmux windows
tmux list-windows -t "$SESSION" -F '#{window_name}'

# Diff to find orphans and zombies
```

## Claude Code Hooks

Claude Code hooks can automate parts of the worker lifecycle without explicit skill logic.

### Relevant Hooks

| Hook | When | Use Case |
|------|------|----------|
| **SessionStart** | Session begins/resumes | Load Linear issue context, set environment |
| **PostToolUse** | After Edit/Write succeeds | Auto-commit WIP changes |
| **Stop** | Claude finishes responding | Trigger self-termination check |
| **SessionEnd** | Session terminates | Final cleanup (workspace, Linear status) |

### Example: Stop Hook for Self-Termination

```json
{
  "hooks": {
    "Stop": [{
      "hooks": [{
        "type": "prompt",
        "prompt": "Check if task is complete: tests pass, PR created, Linear updated. Respond {\"ok\": true} to finish or {\"ok\": false, \"reason\": \"...\"} to continue."
      }]
    }]
  }
}
```

### Example: PostToolUse for Auto-Snapshot

```json
{
  "hooks": {
    "PostToolUse": [{
      "matcher": "Edit|Write",
      "hooks": [{
        "type": "command",
        "command": "jj status >/dev/null 2>&1 || true"
      }]
    }]
  }
}
```

Note: `jj status` triggers jj's automatic working copy snapshotting without creating explicit commits. Changes accumulate in the working copy until the worker completes.

## Entry Points

### Daemon

The Daemon is the entry point for the system.

```
ralph-dev start PROJECT_ID    # Start the swarm
ralph-dev stop PROJECT_ID     # Stop the swarm
ralph-dev status PROJECT_ID   # Check health
```

**Daemon responsibilities:**

1. Create tmux session (`ralph-$PROJECT`)
2. Start Controller in dedicated window
3. Run health loop (every ~3 min):
   - Check Controller heartbeat
   - Spawn Supervisor (ephemeral) to reason about health if stale
4. Handle graceful shutdown

**Sketch:**

```
Daemon starts
    │
    ├── Create tmux session
    ├── Start Controller window
    │       └── claude -p "Use ralph-dev-controller skill. Project: $PROJECT"
    │
    └── Health loop (every 3 min):
            ├── Check heartbeat file age
            └── If stale → spawn Supervisor
                    └── claude -p "Use ralph-dev-supervisor skill. Project: $PROJECT"
```

### Supervisor (ralph-dev-supervisor skill)

- Ephemeral — fresh context each spawn
- Reasons about controller + worker health
- Decides: restart, nudge, or leave alone
- Spawned by Daemon when health check triggers

### Controller (ralph-dev-controller skill)

- Ephemeral — Ralph loop (fresh context each iteration)
- Polls Linear for state
- Routes tasks to appropriate worker skills
- Spawns workers, merges completed workspaces

## Skills Structure

| Skill | Purpose |
|-------|---------|
| `ralph-dev-supervisor` | Reason about health, restart/nudge components |
| `ralph-dev-controller` | Poll Linear, route tasks, spawn workers, merge |
| `ralph-dev-execute` | Implement code changes |
| `ralph-dev-plan` | Break down phase into tasks |
| `ralph-dev-review` | Code review |
| `ralph-dev-resolve` | Handle merge conflicts |
| `ralph-dev-research` | Investigate open questions |

## Dependencies

- **linear-cli** (`@schpet/linear-cli`) - Linear API operations, jj integration
- **jj** - Version control with workspaces
- **tmux** - Process management
- **Claude Code** - Agent runtime

## Component Implementation Decisions

### Building Blocks

The system uses these Claude Code primitives:

| Primitive | What it is | Use when |
|-----------|-----------|----------|
| **Skill** | Markdown instructions loaded into a Claude session | Reusable workflows, behavior guidance |
| **Subagent** | Spawned via Task tool, returns result to parent | Short-lived work within a session |
| **Independent process** | Separate `claude` command in own tmux window | Parallel execution, isolation |
| **Hook** | Shell command triggered by Claude Code events | Automation without LLM reasoning |

### Component Breakdown

| Component | Implementation | Lifecycle | Responsibilities |
|-----------|---------------|-----------|------------------|
| **Daemon** | Persistent process | Long-running | Start controller, run health loop, spawn supervisor |
| **Supervisor** | Skill (ephemeral) | Fresh each health check | Reason about controller + worker health, restart/nudge as needed |
| **Controller** | Skill (ephemeral) | Ralph loop | Poll Linear, route tasks to skills, spawn workers, merge results |
| **Worker** | Independent Claude process + skill | Ephemeral, may iterate | Execute assigned skill |
| **Auto-commit** | Hook (PostToolUse) | Automatic | `jj status` to snapshot working copy |
| **Self-termination** | Hook (Stop) | Automatic | Cleanup on worker completion |

### Design Rationale

**Daemon (persistent process):**
- Entry point for the system (`ralph-dev start PROJECT`)
- Handles mechanical health checks (heartbeat staleness, tmux window existence)
- Spawns Supervisor periodically for reasoning about health
- More observable than cron, extensible for future init/health logic

**Supervisor (ephemeral skill):**
- Fresh context each spawn — no accumulated state
- Reasons about ambiguous situations (stuck vs slow worker, nudge vs kill)
- Absorbs "Boot" triage role from Gastown — no need for separate Boot + Supervisor

**Controller (ephemeral skill, Ralph loop):**
- Routes tasks to appropriate worker skills (following Gastown's Mayor pattern)
- Workers are generic executors; Controller has the routing intelligence
- Fresh context each iteration prevents context bloat

**Worker (independent process + skill):**
- Runs in own tmux window with own jj workspace (code isolation)
- Assigned a specific skill by Controller (executor, reviewer, resolver, etc.)
- May iterate on a task until complete (Ralph loop style)
- Self-terminates via Hook when done

### Worker Skills

Controller assigns one of these skills when spawning a worker:

| Skill | Purpose |
|-------|---------|
| `ralph-dev-execute` | Implement code changes |
| `ralph-dev-plan` | Break down phase into tasks |
| `ralph-dev-review` | Code review |
| `ralph-dev-resolve` | Handle merge conflicts |
| `ralph-dev-research` | Investigate open questions |

(Extensible — add more skills as needed)

### Hooks

| Hook | Event | Action |
|------|-------|--------|
| Auto-commit | PostToolUse (Edit/Write) | Run `jj status` to snapshot working copy |
| Self-termination | Stop | Update Linear status, clean up jj workspace, close tmux window |
| Issue context | SessionStart | Inject Linear issue details into prompt |

## Open Questions (Deferred)

### Local-First Complement

Explore file-based state management as complement to Linear:
- Easier for agents (no API calls)
- Need good human UX for monitoring/input
- Could sync to/from Linear

## Appendix: Tested Behaviors

| Behavior | Result |
|----------|--------|
| tmux duplicate window names | Allowed (manage ourselves) |
| tmux headless send-keys | Works |
| tmux headless capture-pane | Works |
| Claude `--session-id` | Must be valid UUID |
| Two sessions same UUID | Both start (no lock), write same file |
| Session file location | Based on working directory path |

## Appendix: Linear Rate Limits

| Limit | Authenticated |
|-------|---------------|
| Requests/hour | 5,000 (~83/min) |
| Complexity points/hour | 250,000 |
| Max single query | 10,000 points |

Polling every minute with 10 workers is well within limits.

## Appendix: Prior Art Research (2026-01-30)

Researched existing multi-agent orchestration tools before finalizing design.

### Tools Evaluated

| Tool | Author | Stack | Key Insight |
|------|--------|-------|-------------|
| [Gastown](https://github.com/steveyegge/gastown) | Steve Yegge | Go + Beads ledger | "Propulsion" model, self-terminating workers, git worktrees |
| [Loom](https://github.com/ghuntley/loom) | Geoffrey Huntley | Rust + K8s | Anti-swarm (single-agent Ralph loops), formal state machines |
| [Swarm Gist](https://gist.github.com/kieranklaassen/4f2aba89594a4aea4ad64d753984b2ea) | Kieran Klaassen | JS (Claude Code skill) | File-based task queue, task dependency auto-unblocking |
| [swarm-tools](https://github.com/joelhooks/swarm-tools) | Joel Hooks | TypeScript + libSQL | Event sourcing, Swarm Mail coordination, pattern learning |

### Principles Adopted

- **Self-terminating workers** (from Gastown): Workers clean up their own resources
- **Linear as source of truth** (differentiator): Avoids custom ledger complexity
- **Hooks for lifecycle automation** (from Claude Code): Auto-commit, self-termination
- **Controller pattern retained**: Necessary for spawning ephemeral workers and detecting crashes

### Principles Rejected

- **Propulsion for long-lived workers** (Gastown): Our workers are ephemeral, not waiting
- **Anti-swarm single-agent loops** (Loom): We explicitly want parallelism
- **File-based state** (Klaassen): Linear already provides structured task tracking

### swarm-tools Deep Dive (2026-01-31)

[swarm-tools](https://github.com/joelhooks/swarm-tools) by Joel Hooks is a three-tier coordination framework:

```
Tier 3: Orchestration    ← OpenCode/Claude Code Plugin (hive, swarm, skills, learning)
        ↓
Tier 2: Coordination     ← Actor-model primitives (DurableMailbox, DurableLock, ask<Req,Res>)
        ↓
Tier 1: Primitives       ← DurableCursor, DurableDeferred
        ↓
Storage: libSQL          ← Event sourcing + vector embeddings (sqlite-vec)
```

**Key Components:**

| Component | Purpose | Implementation |
|-----------|---------|----------------|
| **Hive** | Task tracking | Git-backed `.hive/issues.jsonl`, survives sessions |
| **Swarm Mail** | Agent coordination | Event sourcing with 56 typed events, projections for agents/messages/reservations |
| **Hivemind** | Semantic memory | Ollama embeddings + FTS fallback, pattern learning |
| **CASS** | Session search | Indexes transcripts from Claude Code, Cursor, Aider, etc. |

**Comparison with Ralph Dev Swarm:**

| Aspect | swarm-tools | Ralph Dev Swarm |
|--------|-------------|-----------------|
| Task Source | Internal `.hive/` (file-based) | Linear (external API) |
| Coordination | Event sourcing + Swarm Mail | Linear comments + tmux send-keys |
| Code Isolation | File reservations (soft locks) | jj workspaces (hard isolation) |
| Process Mgmt | OpenCode sessions | tmux windows |
| Learning | Built-in pattern system | Potential addition (see below) |

**What We Could Adopt:**

1. **Swarm Mail for inter-agent messaging** — More reliable than tmux send-keys for complex coordination
2. **Event sourcing for observability** — Query "what happened" without parsing transcripts
3. **Pattern learning** — Track what decomposition strategies work (see section below)

**What We Keep Different:**

1. **Linear as source of truth** — Workers don't need to know about Swarm Mail; controller mediates
2. **jj workspaces for isolation** — Stronger than file reservations, no coordination needed
3. **Post-hoc transcript analysis** — Claude Code already stores transcripts; embed/index on demand

## Appendix: Inter-Agent Communication Design

### Communication Architecture

All inter-agent communication flows through the Controller:

```
                    ┌──────────────┐
                    │   Linear     │  ← Human-visible state
                    └──────┬───────┘
                           │
                    ┌──────▼───────┐
                    │  Controller  │  ← Routes messages, summarizes updates
                    └──────┬───────┘
                           │
            ┌──────────────┼──────────────┐
            │              │              │
     ┌──────▼───────┐ ┌────▼────┐ ┌───────▼──────┐
     │   Worker A   │ │Worker B │ │   Worker C   │
     └──────────────┘ └─────────┘ └──────────────┘
```

**Why Controller-mediated:**
- Controller can summarize verbose status updates before posting to Linear
- Controller has visibility into all worker state for routing decisions
- Workers stay simple (no Linear API calls needed for communication)
- Controller can intervene via tmux send-keys as escape hatch

### Communication Options Considered

| Option | Mechanism | Pros | Cons |
|--------|-----------|------|------|
| **Linear comments** | Worker → Linear API | Human-visible, persistent | Requires API calls from workers |
| **Swarm Mail** | Event log in libSQL | Structured events, queryable | New dependency, workers need swarm-mail |
| **File-based mailbox** | JSON files in shared dir | Simple, no dependencies | Polling overhead, no typing |
| **Controller relay** | Worker → Controller → Linear | Workers stay simple | Single point of routing |

**Decision:** Controller relay with Linear as persistence layer. Workers report to Controller (via completion status or comments), Controller posts to Linear. For urgent intervention, Controller uses tmux send-keys.

### Observability Without Swarm Mail

Claude Code already provides observability:
- **Session transcripts**: `~/.claude/projects/-path-to-workspace/*.jsonl`
- **Hooks can emit events**: PostToolUse hooks can append to a shared log

tmux-based observability stack:

```
~/.ralph/
├── events.jsonl      # Append-only event log (from Claude Code hooks)
├── logs/
│   ├── ENG-21.log    # Full terminal output (from pipe-pane)
│   └── ENG-22.log
└── state/
    ├── ENG-21.json   # Current worker state (worker-written)
    └── ENG-22.json
```

**Query patterns:**

| Question | Command |
|----------|---------|
| What's everyone doing? | `cat ~/.ralph/state/*.json \| jq -s` |
| What happened to ENG-21? | `jq 'select(.agent=="ENG-21")' ~/.ralph/events.jsonl` |
| Full worker transcript | `cat ~/.ralph/logs/ENG-21.log` |

## Appendix: Pattern Learning Design

### Overview

Track which decomposition strategies work well and which don't. This is simpler than machine learning — just counters on a predefined vocabulary.

### How It Works

**1. Predefined Strategy Vocabulary:**

Patterns are a fixed list of known decomposition strategies, detected via regex:

| Strategy | Regex Pattern |
|----------|---------------|
| Split by file type | `/split(?:ting)?\s+by\s+file\s+type/i` |
| Split by component | `/split(?:ting)?\s+by\s+component/i` |
| One file per subtask | `/one\s+file\s+per\s+(?:sub)?task/i` |
| Shared types first | `/shared\s+types?\s+first/i` |
| Tests in separate task | `/tests?\s+(?:in\s+)?separate\s+(?:sub)?task/i` |
| Maximize parallelization | `/parallel(?:ize)?\s+(?:all\|everything)/i` |

**2. Pattern Lifecycle:**

```
candidate ──────────────────────────────────────────────→ deprecated
    │                                                        ↑
    │ (>= 3 observations)                                    │
    ↓                                                        │
established ────────────────────────────────────────────→ deprecated
    │                                                        ↑
    │ (>= 5 helpful, < 15% harmful)                          │
    ↓                                                        │
  proven ──────────────────────────────────────────────→ deprecated
                                      (> 30% harmful)
```

| State | Meaning | Prompt weight |
|-------|---------|---------------|
| candidate | < 3 observations | 0.5x (penalized) |
| established | >= 3 observations | 1.0x (neutral) |
| proven | >= 5 helpful, < 15% harmful | 1.5x (boosted) |
| deprecated | > 30% harmful ratio | 0x (excluded) |

**3. When Patterns Are Recorded:**

- On task completion: Extract strategies from task description via regex
- Record success (task completed) or failure (task failed/blocked)
- Update pattern counts and maturity state

**4. Time Decay:**

Observations decay with 90-day half-life:
- Recent feedback counts more than old feedback
- Prevents stale patterns from dominating

### Implementation (Minimal)

A JSON file is sufficient:

```json
{
  "patterns": {
    "split-by-file-type": {
      "state": "established",
      "helpful_count": 5,
      "harmful_count": 2,
      "last_validated": "2026-01-30T12:00:00Z"
    },
    "tests-in-separate-task": {
      "state": "deprecated",
      "helpful_count": 1,
      "harmful_count": 4,
      "last_validated": "2026-01-30T12:00:00Z"
    }
  }
}
```

**Usage in prompts:**

```markdown
## Proven Patterns
These decomposition strategies have worked well:
- Split by file type [PROVEN - 83% helpful from 6 observations]

## Deprecated Patterns
AVOID these patterns - they have poor track records:
- Tests in separate subtask [DEPRECATED - 80% harmful, avoid using]
```

### Comparison with swarm-tools Learning

| Layer | swarm-tools | Ralph Dev Swarm |
|-------|-------------|-----------------|
| **Anti-pattern tracking** | Counters + auto-invert at 60% | Same (simpler threshold) |
| **Vector search (CASS)** | Ollama embeddings in libSQL | Post-hoc via Claude Code transcripts |
| **Smart memory (Mem0)** | LLM decides ADD/UPDATE/DELETE | Not needed for our use case |

We can implement pattern learning with just a JSON file — no embeddings, no LLM calls, no new infrastructure.
