# Legion: Skills-First Roadmap

**Date:** 2026-02-11
**Status:** Ready for review
**Supersedes:** 2026-02-06-orchestration-roadmap.md (partially), .sisyphus/plans/pipeline-discipline.md, .sisyphus/plans/continuation-stack.md, .sisyphus/drafts/model-optimized-prompt-architecture.md

---

## Strategic Reset

We completed a major refactor from Python to TypeScript/OpenCode, then started copying oh-my-opencode's agents and hooks into the plugin. That went off track. The plugin was growing toward a second oh-my-opencode — 11 agents, 40+ hooks, tools duplicating OpenCode native behavior.

**New direction:** Skills own workflow intelligence. The plugin is a thin substrate providing only what skills can't.

### Design Principles

1. **Skills-first, progressive disclosure.** Workflow behavior lives in skill markdown (e.g. `/superpowers:*`, `/compound-engineering:*`). The plugin does not contain agent prompts or behavioral hooks that compete with skills.
2. **Sessions-only.** Every delegation is a full OpenCode session — observable, attachable, traceable via `parentID`. No opaque "background jobs."
3. **Categories separate concerns.** A category defines permissions + system prompt (task type). Model selection is a separate, per-invocation parameter. Don't conflate them.
4. **Thin plugin substrate.** The plugin exists only for capabilities that skills alone cannot provide (see "Thin Substrate Test" below).

### Thin Substrate Test

Each plugin feature must pass this test: **could a skill achieve this with only OpenCode native tools?**

| Feature | Plugin-only? | Why |
|---------|-------------|-----|
| Cross-model routing | Yes | Skills can't change which model processes a session at runtime |
| Subagent session spawning + parentID tracking | Yes | Skills can't subscribe to `session.status` events or manage session lifecycle |
| Permission enforcement (read-only reviewers) | Yes | Skills say "please don't edit" — an LLM can ignore this; plugin denies at tool layer |
| Session recovery (error event handling) | Yes | Skills can't intercept `session.error` events |
| Persistent task system with locking | Borderline | Implementable via bash+files, but plugin guarantees deterministic behavior under concurrency |
| Essential hooks (effort injection, thinking validation, non-interactive env) | Yes | Skills can't intercept `chat.params` pipeline |

### What Gets Removed / Not Ported

- **11 custom agent prompts** — collapse to minimal set; skills provide specialization
- **Most hooks** (auto-slash-command, label truncation, context-window-monitor, atlas enforcement, prometheus-md-only, sisyphus-junior-notepad, keyword-detector, etc.)
- **Continuation stack** (.sisyphus boulder/todo-continuation) — deferred; revisit only if OpenCode native compaction + summarization proves insufficient
- **Tool duplication** (grep, lsp, glob wrappers) where OpenCode provides equivalent native tools

---

## Current State

### What Works

- Controller daemon polls Linear every 30s, dispatches workers via HTTP API
- Worker modes: architect → plan → implement → review → retro → merge
- jj workspace isolation (up to 10 parallel workers)
- Skills invoke `/superpowers:*` and `/compound-engineering:*` for planning, TDD, review
- Plugin provides: session spawning (`BackgroundTaskManager`), category→model routing, permission gating, session tools

### Known Issues

- Controller skill (`SKILL.md`) references `bun run src/state/cli.ts` — wrong path; actual code is at `packages/daemon/src/state/cli.ts`
- Plugin has accumulated agent/hook/tool surface area that duplicates OpenCode native + skill capabilities
- No cross-family review (same model reviews its own work)
- No dependency-aware task management (no parallel subagent execution with safe work distribution)
- `implement.md` doesn't enforce green CI (tests + types pass) as exit condition before review dispatch
- Categories conflate task type (permissions) with model selection

---

## Phase 0: Correctness + Alignment

*Unbreak what's broken. Align code to the new direction. Days, not weeks.*

### 0.1 Fix controller → state CLI path

The controller skill invokes `echo "$LINEAR_JSON" | bun run src/state/cli.ts ...` but the state CLI is at `packages/daemon/src/state/cli.ts`. Fix the skill and verify the invocation works with correct cwd.

**Scope:** Skill file fix + verify.

### 0.2 Audit and trim plugin surface

Produce a concrete cut list. For each agent/hook/tool in `packages/opencode-plugin/src/`:
- **Keep** if it passes the thin substrate test
- **Remove** if skills or OpenCode native provide equivalent capability
- **Defer** if it's useful but not needed until later phases

**Scope:** Audit doc → code removal PR.

### 0.3 Separate category from model selection

Refactor `category-router.ts` so categories define permissions + system prompt, and model is a separate per-invocation parameter (with category providing a default that can be overridden).

**Scope:** Plugin code change.

---

## Phase 1: Thin Substrate

*The core plugin capabilities that skills depend on. The foundation everything else builds on.*

### 1.1 Dependency-Aware Task System

Port oh-my-opencode's task system (`~/oh-my-opencode/original/src/features/claude-tasks/` + `~/oh-my-opencode/original/src/tools/task/`) with hardening.

#### Schema

```typescript
interface Task {
  id: string;              // "T-{uuid}"
  subject: string;         // Imperative: "Add error handling to auth module"
  description: string;
  status: "pending" | "in_progress" | "completed" | "cancelled";
  blocks: string[];        // Task IDs this task blocks
  blockedBy: string[];     // Task IDs blocking this task
  owner?: string;          // Session/agent that claimed it
  metadata?: Record<string, unknown>;  // Includes lease_expires_at, claimed_by_session, attempt_count
  threadID: string;        // Session that created it
  parentID?: string;       // Parent task ID (for grouping)
}
```

Key delta from OMO: `deleted` status replaced with `cancelled` (satisfies dependencies — prevents dead chains).

#### Storage

- One JSON file per task: `T-{uuid}.json`
- Atomic writes: write temp + rename (port from OMO `writeJsonAtomic`)
- Coarse `.lock` file per task list with stale threshold
- Task list scoped to issue+mode via env var `CLAUDE_CODE_TASK_LIST_ID` (daemon sets `${LINEAR_ISSUE_ID}-${mode}`)

#### Tools

| Tool | Description |
|------|-------------|
| `task_create` | Create task with optional `blockedBy`/`blocks`; returns `{id, subject}` |
| `task_get` | Full task by ID |
| `task_update` | Additive deps (`addBlockedBy`/`addBlocks`), status, owner, metadata merge; syncs to OpenCode todos |
| `task_list` | List active tasks; `ready=true` filters to tasks whose blockedBy are all completed/cancelled |
| `task_claim_next` | Atomic claim: filter ready+pending, pick deterministically, set in_progress+owner+lease |

#### Hardening (from review feedback)

- **Cycle detection:** On `task_create` and `task_update` with `addBlockedBy`/`addBlocks`, check for cycles in the dependency graph. Reject with error if cycle detected.
- **Referential integrity:** Warn (don't reject) when `blockedBy` references a task ID that doesn't exist. Missing deps = blocked (safe default).
- **Cancelled satisfies deps:** A `cancelled` task counts as "completed" for ready-check purposes. This prevents dead dependency chains.
- **Retry cap:** Track `metadata.attempt_count` on each claim. After 3 failed attempts (claimed then abandoned/expired), mark task for escalation rather than re-claiming.
- **Lease semantics:** `task_claim_next` sets `metadata.lease_expires_at` and `metadata.claimed_by_session`. Expired leases are auto-reclaimed on next claim pass. Optional `task_heartbeat` to extend lease for long tasks.

#### Todo Sync

Mirror tasks into OpenCode session todos for UI visibility (port from OMO `todo-sync.ts`):
- `id` → `todo.id`, `subject` → `todo.content`, status mapped, priority from `metadata.priority`
- Tasks are source of truth; todos are one-way mirror
- Sync on create/update

#### Sources to Port From

| Component | Source | Est. LOC |
|-----------|--------|----------|
| Task types/schema | `~/oh-my-opencode/original/src/tools/task/types.ts` | ~80 |
| Storage utilities | `~/oh-my-opencode/original/src/features/claude-tasks/storage.ts` | ~170 |
| task_create | `~/oh-my-opencode/original/src/tools/task/task-create.ts` | ~115 |
| task_get | `~/oh-my-opencode/original/src/tools/task/task-get.ts` | ~50 |
| task_update | `~/oh-my-opencode/original/src/tools/task/task-update.ts` | ~155 |
| task_list | `~/oh-my-opencode/original/src/tools/task/task-list.ts` | ~80 |
| todo sync | `~/oh-my-opencode/original/src/tools/task/todo-sync.ts` | ~175 |
| **New:** task_claim_next | — | ~100 |
| **New:** cycle detection | — | ~50 |

### 1.2 Sessions-First Subagent Spawning

Keep existing `BackgroundTaskManager` + delegation tools, but reframe UX:
- Tool output should emphasize "this created a session" (return sessionID, attach hint)
- Ensure `parentID` linkage is always set
- Return `sessionID` prominently so parent can inspect/attach/read

#### Reviewer Categories

| Category | Permissions | Purpose |
|----------|-------------|---------|
| `review-architect` | read-only (deny edit/bash) | Review architect output before worker-done |
| `review-plan` | read-only | Review plan output before posting + worker-done |
| `review-implementation` | read-only | Review implementation before shipping PR |

Model assignment is per-invocation, not per-category. Categories define permissions + system prompt template.

Future (not now): `review-spec` and `review-quality` for 2-pass reviews per phase.

#### Permission Enforcement

Reviewer sessions MUST have write/edit/shell denied at the tool layer, not just prompt policy. The plugin enforces this via the existing permission config pattern:

```typescript
// Reviewer permission profile
{ edit: "deny", bash: "deny", write: "deny" }
```

### 1.3 Essential Hooks (keep list)

Only hooks that pass the thin substrate test:

| Hook | Why plugin-only |
|------|----------------|
| `non-interactive-env` | GIT_EDITOR=: etc. so git doesn't hang in autonomous mode |
| `subagent-question-blocker` | Prevents background sessions from asking humans |
| `preemptive-compaction` | Model-aware context limit management |
| `stop-continuation-guard` | Prevents auto-continue after user stop |

All other hooks are removed or deferred.

### 1.4 Implement Workflow: Enforce Green CI

Update `implement.md` to require tests + type checks pass before shipping PR:

```
### Pre-Ship Verification
bun test          # Must pass
bunx tsc --noEmit # Must pass
bunx biome check  # Must pass
```

This is a precondition for review dispatch — don't waste a review cycle on code that doesn't compile.

---

## Phase 2: Cross-Family Review

*Every phase output gets a cross-family review before finalizing. Reviews are separate sessions.*

### 2.1 Architect Review Step

After architect drafts acceptance criteria / sub-issues:
1. Worker spawns a `review-architect` session (cross-family via category model selection)
2. Reviewer reads the architect output and the original issue
3. Reviewer produces structured feedback (acceptance criteria quality, completeness, testability)
4. Worker incorporates feedback
5. Only then adds `worker-done`

**Scope:** Update `architect.md` workflow.

### 2.2 Plan Review Step

After the existing internal review loop (`/compound-engineering:plan_review`):
1. Worker spawns a `review-plan` session (cross-family)
2. Reviewer evaluates plan against requirements, feasibility, dependency structure
3. Worker incorporates feedback
4. Posts final plan + adds `worker-done`

**Scope:** Update `plan.md` workflow.

### 2.3 Implementation Review Step

Before shipping PR (after local CI passes):
1. Worker spawns a `review-implementation` session (cross-family)
2. Reviewer reads diff, plan, requirements — evaluates spec compliance + code quality
3. Worker addresses findings
4. Ships PR

This runs in addition to the existing `review` mode (which is a separate worker dispatched by the controller after PR is opened). The intra-worker cross-family review catches issues before the PR even exists.

**Scope:** Update `implement.md` workflow.

### 2.4 Multi-Session Team Execution

For implementation tasks with independent subtasks:
1. Parent worker creates dependency graph via `task_create` (with `blockedBy`/`blocks`)
2. Parent spawns N subagent sessions
3. Each subagent loops: `task_claim_next(ready=true, owner=<session>)` → execute → `task_update(status="completed")`
4. Lock prevents double-claiming; lease prevents dead tasks from crashed subagents
5. Parent monitors progress, spawns cross-family reviewer when all tasks complete

**Scope:** Update `implement.md` to use task system for parallel execution when plan has independent tasks.

---

## Phase 3: Pipeline Discipline

*Human approval gates, structured escalation, quality gates. Implemented in daemon/state + skills, not plugin hooks.*

### 3.1 Human Approval Gate (LEG-66)

After architect produces requirements, controller pauses for human approval before dispatching planner.

- Add `needs-approval` / `human-approved` labels to state machine
- Controller checks labels in `buildIssueState()` pre-check (follows existing `hasUserInputNeeded` pattern)
- No auto-approve mechanism

**Scope:** State machine change (`packages/daemon/src/state/`) + controller skill update.

### 3.2 Structured Escalation (LEG-70)

When workers escalate with `user-input-needed`, they use a structured template:
- Phase, completed work, blocker, options considered, remaining estimate, expertise needed

**Scope:** Template in worker SKILL.md or reference doc.

### 3.3 Pre-Transition Quality Gates (LEG-86)

Before controller advances In Progress → Needs Review, verify the worker's branch passes basic quality checks (biome + tsc + bun test).

**Scope:** Controller behavior change + state machine parameter.

### 3.4 Upfront Parallelism Annotation (LEG-71)

Plan workflow annotates task dependencies and parallelizability explicitly. This feeds directly into the Phase 1 task system — the plan output becomes the task graph input for the implementer.

**Scope:** New step in `plan.md`.

---

## Deferred

### Continuation Stack (.sisyphus boulder/todo-continuation)

**Status:** Explicitly deferred.
**Re-entry criteria:** Repeated incidents where OpenCode native compaction + preemptive-compaction hook loses critical work state that the task system doesn't preserve.

### Planning Depth (LEG-87, LEG-67, LEG-88)

Codebase index, impact analysis, proactive memory injection. Still valid from the original roadmap but deferred until Phases 0-2 are solid.

### Workflow Adaptability (LEG-90, LEG-91)

Dynamic workflow orchestration, model-determined topology. Valid but later — make the existing pipeline excellent before making it flexible.

### System Intelligence

Context health monitoring, within-family model tiering, cost tracking, knowledge graph. Exploratory.

---

## GitHub Issue Reconciliation

| Issue | Original Phase | New Status | Notes |
|-------|---------------|------------|-------|
| LEG-66 | Pipeline Discipline | Phase 3.1 | Unchanged; approval gate in state machine |
| LEG-67 | Planning Depth | Deferred | Depends on LEG-87 (codebase index) |
| LEG-68 | Verification | Phase 2 | Cross-family review, now sessions-based |
| LEG-69 | Verification | Deferred | E2E testing capability |
| LEG-70 | Pipeline Discipline | Phase 3.2 | Structured escalation template |
| LEG-71 | Pipeline Discipline | Phase 3.4 | Parallelism annotation feeds task system |
| LEG-86 | Pipeline Discipline | Phase 3.3 | Pre-transition quality gates |
| LEG-87 | Planning Depth | Deferred | Codebase index |
| LEG-88 | Planning Depth | Deferred | Proactive memory injection |
| LEG-89 | Verification | Deferred | Stuck detection |
| LEG-90 | Adaptability | Deferred | Dynamic workflow orchestration |
| LEG-91 | Adaptability | Deferred | Model-determined topology |

### New Work (not in original roadmap)

| Item | Phase | Description |
|------|-------|-------------|
| Fix controller state CLI path | 0.1 | Correctness fix |
| Audit + trim plugin surface | 0.2 | Remove accumulated bloat |
| Separate category from model selection | 0.3 | Architecture fix |
| Dependency-aware task system | 1.1 | Port from OMO + hardening |
| Sessions-first subagent spawning | 1.2 | Reframe existing tools |
| Reviewer categories + permissions | 1.2 | Read-only enforcement |
| Essential hooks audit | 1.3 | Keep only 4 hooks |
| Enforce green CI before review | 1.4 | Implement workflow fix |
| Cross-family review per phase | 2.1-2.3 | Architect, plan, implementation |
| Multi-session team execution | 2.4 | Task system + subagent teams |

---

## Review Findings (Incorporated)

This roadmap was reviewed by Oracle and Ultrabrain agents. Key findings incorporated:

1. **Cycle detection** in dependency graph — added to task system hardening (Phase 1.1)
2. **`deleted` → `cancelled`** status that satisfies dependencies — prevents dead chains (Phase 1.1)
3. **Retry cap** per task — max 3 claims before escalation (Phase 1.1)
4. **Lease semantics** with explicit `claimed_by_session` and `lease_expires_at` (Phase 1.1)
5. **Separate category from model selection** — promoted to Phase 0.3 (Phase 0.3)
6. **Green CI as implementer exit condition** — added as Phase 1.4
7. **Read-only enforcement at tool layer** not just prompt policy — specified in Phase 1.2

### Findings Noted but Not Acted On

- **Per-subagent `jj new` for workspace isolation:** Not needed. File separation is a planning convention.
- **Batch assignment instead of task system:** Task system kept in Phase 1; there are straightforward examples to port from OMO.
- **Single task list per issue (vs per issue+mode):** Keeping issue+mode scoping as decided.
- **"Verifier" role (shell allowed, write denied):** Interesting but not needed now; reviewers are static-analysis-only, CI covers runtime verification.

---

## Prioritization Rationale

| Phase | Effort | Impact | Why this order |
|-------|--------|--------|----------------|
| 0. Correctness | Low | High | Unblocks everything; fixes broken paths and removes bloat |
| 1. Thin Substrate | Medium | High | Foundation: task system + sessions + categories + permissions |
| 2. Cross-Family Review | Medium | High | Quality: every phase output reviewed by different model family |
| 3. Pipeline Discipline | Medium | Medium | Safety: human gates, escalation, quality checks |
| Deferred | — | — | Revisit after Phases 0-2 are solid |
