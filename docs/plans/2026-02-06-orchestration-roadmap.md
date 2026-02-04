# Legion Orchestration Roadmap

**Date:** 2026-02-06
**Status:** Ready for review
**Context:** Based on comparative analysis of Blitzy, Oh My OpenCode, Claude Code Agent Teams, community reports, and our design principles.

---

## Current State

Legion has a working orchestration pipeline:
- Controller daemon polling Linear every 30s
- Worker modes: architect → plan → implement → review → retro → merge
- jj workspace isolation (up to 10 parallel workers)
- TDD enforcement via /superpowers:test-driven-development
- Multi-agent plan review (up to 3 iterations)
- Dual-perspective retro capturing learnings to docs/solutions/
- Oracle sub-skill for research before human escalation

## What's Missing

From the research, the gaps fall into four categories:
1. **Pipeline discipline** — The pipeline auto-advances without human checkpoints on requirements, escalation is unstructured, and parallelism is ad-hoc
2. **Planning depth** — No impact analysis, no codebase index, learnings aren't proactively injected
3. **Verification breadth** — Same-model review, no E2E testing, no pre-transition quality gates
4. **Workflow adaptability** — Fixed pipeline for every issue regardless of complexity

---

## Phase 1: Pipeline Discipline

*Make the existing pipeline more robust. Highest leverage, lowest effort.*

### LEG-66: Human approval gate on architect outputs
**Principle:** *Humans decide what, agents decide how*

After the architect produces requirements (acceptance criteria, sub-issues), the controller pauses for human approval before dispatching the planner. This is the single highest-leverage change — bad requirements cascade through every subsequent phase.

**Scope:** Controller state transition change + new label (`needs-approval` / `approved`). No new workers or skills.

### LEG-70: Structured escalation reports
**Principle:** *Failure is an output, not a bug*

When workers escalate with `user-input-needed`, they use a structured template: phase, completed work, blocker, options considered, remaining estimate, expertise needed. Replace ad-hoc Linear comments with a first-class handoff document.

**Scope:** Template added to worker SKILL.md or a new sub-skill alongside oracle.

### LEG-71: Upfront parallelism annotation in plans
**Principle:** *Planning is the product*

The plan workflow annotates task dependencies and parallelizability explicitly. Default to sequential when uncertain. The implementer can override with its own analysis.

**Scope:** New step in plan.md between /superpowers:writing-plans and posting to Linear.

### New: Pre-transition quality gates
**Principle:** *Every claim must be verified independently*

Before the controller advances from In Progress → Needs Review, verify that the worker's branch passes basic quality checks (ruff, basedpyright, pytest). Don't waste a review cycle on code that doesn't compile.

**Scope:** Controller runs quality checks in the worker's workspace before state transition. Add `tests_passing` parameter to `suggest_action()`.

---

## Phase 2: Planning Depth

*Strengthen the planning phase to catch more issues before code is written.*

### LEG-87: Lightweight codebase index
**Principle:** *Context is the bottleneck, not intelligence*

Build a module dependency graph + file-to-test mapping + public API surface + recent change hotspots at `legion start`. Updated incrementally by the daemon. Workers query it alongside Grep/Glob/Read.

This is the 80/20 version of Blitzy's knowledge graph — not days of compute, but enough to make impact analysis fast and accurate. This is a prerequisite for effective impact analysis — without it, impact analysis is ad-hoc grepping.

**Scope:** JSON/SQLite file in the workspace. Script runs at startup, updated between controller loops. Workers get a new tool to query it.

### LEG-67: Impact analysis step in plan workflow
**Principle:** *Context is the bottleneck, not intelligence*

**Depends on:** LEG-87 (codebase index)

Add an impact analysis agent to the plan workflow that:
- Queries the codebase index for dependencies of affected files
- Maps imports/callers of affected modules
- Checks if the plan accounts for updating dependents
- Uses `jj log --file` for recent change hotspots
- Feeds gaps into the plan review as findings

**Scope:** New agent dispatch between /deepen-plan and /plan_review. Agent queries the codebase index and uses Grep/Glob for anything not indexed.

### New: Proactive memory injection
**Principle:** *Knowledge compounds*

Build a file-path → learnings index. When the plan workflow identifies which files will be touched, automatically inject relevant `docs/solutions/` entries into the implementer's context. Shift from "search when stuck" to "preload what's relevant."

The index is maintained by the retro workflow: when a learning is written, retro updates the index with the files modified in that PR.

**Scope:** JSON index file. Retro workflow updates it. Plan workflow reads it and includes relevant learnings in the plan posted to Linear.

---

## Phase 3: Verification Breadth

*Catch what same-model review and unit tests miss.*

### LEG-68: Cross-family model review via OpenCode
**Principle:** *Use the best tool for each job*

Add at least one non-Claude review pass to the review workflow. OpenCode CLI is the most viable integration: `opencode -p "prompt" --model google/gemini-3-pro -f json`.

**Phased approach:**
1. POC: Single OpenCode review pass with GPT-4o, parse JSON output, post to PR
2. Multi-model: Parallel reviewers (security, performance, general) across model families
3. Synthesis: Deduplicate findings, confidence scores based on cross-model agreement

**Scope:** New step in review.md. Requires OpenCode installed in worker environments.

### LEG-69: E2E testing capability set
**Principle:** *Composable capabilities, not fixed specialists*

Build a testing capability set (skill + tools) that any worker can load, rather than a dedicated tester worker type. Per Principle 9: if adding a capability requires a new worker mode and state machine update, the specialization is too rigid.

The architect defines E2E test scenarios, the planner includes test steps, and the controller dispatches a worker with the testing capability loaded. The capability includes Playwright MCP (or agent-browser), subprocess runners, and evidence capture.

**Scope:** New skill (`skills/testing-e2e/SKILL.md`), tool integrations. No new worker mode in types.py — testing is a loadable capability, not a specialist.

### New: Active-but-unproductive stuck detection
**Principle:** *Failure is an output, not a bug*

Current stuck detection kills workers after 10 min of inactivity. Missing: detecting workers that are active but looping (oscillating between fixing one test and breaking another, like Blitzy describes).

Heuristics from Factory's research: self-rewriting plans, edits outside scope, claims without reproductions, bloated diffs. Monitor session file growth patterns — if a worker has been active for 30+ minutes with a very large session file, it may be stuck.

**Scope:** Enhancement to `check_worker_health()` in daemon.py. Add session size / duration heuristics alongside existing staleness checks.

---

## Phase 4: Workflow Adaptability

*Make the system smarter about matching workflow to work. Note: this is Phase 4 in implementation order, but it's an architectural constraint from day one — every Phase 1-3 item should be built as a removable hook, not a permanent state transition.*

### LEG-90: Dynamic workflow orchestration
**Principle:** *Structure should be thin and model-determined*

The fixed pipeline (architect → plan → implement → review → retro → merge) is a routing template, not the architecture. The goal is a system where the model determines the workflow topology at runtime, with the current pipeline as the default.

**Level 1 — Skip conditions (near-term):**
Every workflow step has explicit skip criteria. The controller evaluates them. Example: if the issue has concrete code examples in acceptance criteria and touches ≤3 files, skip planning and go straight to implement.

**Level 2 — Model-determined routing (medium-term):**
The architect's output includes workflow hints — not just requirements, but a structured signal about what phases the issue actually needs. The controller routes based on those hints. This moves workflow design from the harness into the model.

**Level 3 — Worker self-rerouting (longer-term):**
Workers can signal re-routing ("this is bigger than planned," "this is simpler than expected"). The controller re-routes: send back to architect for breakdown, dispatch sub-agents for parallel sub-tasks, or skip remaining phases.

**The Bitter Lesson constraint:** Every new workflow feature (quality gates, approval gates, E2E testing) must be implemented as a hook or configurable check that can be turned off with a config change. If removing a feature requires modifying the state machine, it's too tightly coupled.

**Scope:** Design doc first. But the architectural constraint applies now: Phase 1-3 implementations should use hooks/config, not hardcoded state transitions.

### LEG-91: Model-determined workflow topology
**Principle:** *Structure should be thin and model-determined*

Instead of pre-defined complexity categories (`quick`/`standard`/`complex`), the architect outputs structured workflow hints that the controller uses for routing. The model decides what the issue needs — the harness doesn't pre-define the options.

The harness enforces resource constraints (max agents, max compute budget, required product gates like human approval). But it doesn't decide which workflow steps an issue needs.

**Scope:** Architect output format change + controller routing logic. No pre-defined categories in the codebase.

---

## Phase 5: System Intelligence (Future)

*Deeper codebase understanding, context management, cost optimization. Exploratory.*

### Context health monitoring
Detect when workers approach context limits or exhibit anxiety symptoms (giving up, fabricating time constraints, returning true to pass tests). Pattern-match worker output via `tmux capture-pane` for known anxiety phrases.

### Within-family model tiering
Use cheaper/faster models for exploration and setup (Haiku), standard models for implementation (Sonnet), strongest models for review and debugging (Opus). Research showed even within-family diversity helps — Sonnet loops on problems Opus solves in 2 prompts.

### Cost/token tracking per issue type
Model Legion's token costs per issue type to optimize. Anthropic's research: multi-agent = 15x tokens for 90% improvement. Understand where our spend goes and whether it's justified.

### Knowledge graph evolution
If the lightweight codebase index (Phase 2) proves valuable, consider evolving it toward a richer relational index inspired by Blitzy's approach: runtime behavior, cross-service dependencies, production log analysis. This is a major investment — only justified if the simpler approach hits clear limits.

---

## Prioritization Rationale

| Phase | Effort | Impact | Why this order |
|-------|--------|--------|----------------|
| 1. Pipeline Discipline | Low | High | Prevents the most expensive class of mistakes (bad requirements cascading). All skill/template changes, no new infrastructure. |
| 2. Planning Depth | Medium | High | Strengthens the phase where most value is created. Codebase index enables impact analysis and proactive memory. |
| 3. Verification Breadth | Medium | High | Catches what the current pipeline misses. Cross-model review and E2E testing address the two biggest quality gaps. |
| 4. Workflow Adaptability | Medium-High | Medium | Implementation ships after 1–3, but the architectural constraint applies from day one: Phase 1–3 items must be hooks/config, not hardcoded state transitions. |
| 5. System Intelligence | High | Variable | Exploratory. Some items (context monitoring) could be pulled forward if we see specific failures. Others (knowledge graph) are long bets. |

**The guiding principle:** Make the existing pipeline excellent before making it flexible — but build every piece of structure to be removable. Phases 1–3 are quality. Phase 4 is adaptability. Phase 5 is frontier. Phase 4's architectural constraint (thin, removable structure) applies to how Phases 1–3 are implemented.

---

## Issue Summary

### Existing Issues
| Issue | Phase | Title |
|-------|-------|-------|
| LEG-66 | 1 | Human approval gate on architect outputs |
| LEG-70 | 1 | Structured escalation reports sub-skill |
| LEG-71 | 1 | Upfront parallelism annotation in plans |
| LEG-87 | 2 | Lightweight codebase index (dependency graph + file-to-test mapping) |
| LEG-67 | 2 | Impact analysis step in plan workflow (depends on LEG-87) |
| LEG-68 | 3 | Cross-family model review via OpenCode |
| LEG-69 | 3 | E2E testing capability set (toolset, not specialist) |
| LEG-77 | 4 | Dynamic workflow orchestration exploration |

### All Issues (Filed)
| Issue | Phase | Title |
|-------|-------|-------|
| LEG-66 | 1 | Human approval gate on architect outputs |
| LEG-70 | 1 | Structured escalation reports sub-skill |
| LEG-71 | 1 | Upfront parallelism annotation in plans |
| LEG-86 | 1 | Pre-transition quality gates + TDD enforcement |
| LEG-87 | 2 | Lightweight codebase index (prerequisite for impact analysis) |
| LEG-67 | 2 | Impact analysis step in plan workflow (depends on LEG-87) |
| LEG-88 | 2 | Proactive memory injection (file-to-learnings index) |
| LEG-68 | 3 | Cross-family model review via OpenCode |
| LEG-69 | 3 | E2E testing capability set (toolset, not specialist) |
| LEG-89 | 3 | Active-but-unproductive stuck detection |
| LEG-90 | 4 | Dynamic workflow orchestration |
| LEG-91 | 4 | Model-determined workflow topology at triage |
