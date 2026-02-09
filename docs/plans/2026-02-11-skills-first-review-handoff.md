# Review Handoff: Skills-First Roadmap Implementation

**Date:** 2026-02-11
**Purpose:** Deep critical review of the skills-first roadmap implementation
**Plan:** `docs/plans/2026-02-11-skills-first-roadmap.md`

---

## What You're Reviewing

A single large change (104 files, ~6,700 lines) implementing all 15 tasks from the skills-first roadmap across 4 phases. The codebase is a two-layer system: TypeScript daemon (`packages/daemon/`) + OpenCode plugin (`packages/opencode-plugin/`) + skill markdown files (`.claude/skills/`).

Both packages compile cleanly under `bunx tsc --noEmit`. Some test files contain intentionally failing tests that define expected-but-not-yet-implemented behavior.

---

## What Was Done (implemented + reviewed)

### Phase 0: Correctness + Alignment

**0.1 — Fix controller → state CLI path.** Fixed 5 stale `src/state/cli.ts` references to `packages/daemon/src/state/cli.ts` in SKILL.md and AGENTS.md files. The codebase was restructured from flat `src/` to `packages/daemon/src/` and these references were missed.

**0.2 — Audit and trim plugin surface.** Removed:
- 2 hooks: `auto-slash-command`, `question-label-truncator`
- 3 agents: `super-orchestrator`, `builder`, `planner`
- 6 tool directories: `ast-grep/`, `grep/`, `lsp/`, `skill/`, `interactive-bash.ts`, `look-at.ts` (all duplicated OpenCode native tools)

Kept 8 hooks, 8 agents, session tools, delegation tools, overlays, config.

**0.3 — Separate category from model selection.** `CategoryConfig` now has `defaultModel` + `systemPrompt` + `description` + `temperature`. Model is a separate per-invocation parameter via `resolveCategory(category, userConfig, modelOverride)`. Resolution order: explicit model arg > user config override > category defaultModel.

**Design decision made during review:** The plan originally called for categories to define "permissions + system prompt." After implementation and two rounds of deep review, we concluded that LLM-enforced permissions via prompt instructions are sufficient — models follow "do not edit files" instructions reliably. The permission system was removed entirely. Categories now define: model defaults + system prompt + description. The only hard enforcement is `LEAF_AGENTS` in `delegation-tool.ts` which prevents subagents from spawning more subagents.

### Phase 1: Thin Substrate

**1.1 — Dependency-aware task system.** New directory `packages/opencode-plugin/src/tools/task/` with 18 files:
- `types.ts` — Task schema with `cancelled` (not `deleted`) status
- `storage.ts` — Atomic writes (temp+rename), file locking with stale reclaim via atomic rename, `OPENCODE_TASK_LIST_ID` env var with `CLAUDE_CODE_TASK_LIST_ID` fallback
- `graph.ts` — DFS cycle detection on `blockedBy` edges
- `task-create.ts` — Creates tasks, cycle detection on both `blockedBy` and `blocks` directions via virtual edge simulation, bidirectional `blocks`→`blockedBy` sync
- `task-get.ts` — Full task by ID
- `task-update.ts` — Additive deps, cycle detection, bidirectional sync, referential integrity warnings
- `task-list.ts` — Active task listing, `ready` filter (all blockedBy completed/cancelled)
- `task-claim.ts` — Atomic claim with lease semantics, retry cap (3 attempts → escalation), expired lease reclaim with metadata cleanup, todo sync
- `todo-sync.ts` — One-way mirror to OpenCode session todos (best-effort, try/catch)
- `index.ts` — `createTaskTools()` factory
- 8 test files with 78+ tests

Task tools ARE registered in the plugin's tool map (`index.ts`).

**1.2 — Sessions-first subagent spawning.** `BackgroundTaskManager.launch()` creates session synchronously (returns sessionID immediately), starts prompt in background. Tool output emphasizes session ID + attach hint. 3 reviewer categories added (`review-architect`, `review-plan`, `review-implementation`) with system prompts instructing read-only behavior. `parentID` always set.

**1.3 — Essential hooks audit.** All 8 essential hooks verified wired: `anthropic-effort`, `background-notification`, `non-interactive-env`, `preemptive-compaction`, `session-recovery`, `stop-continuation-guard`, `subagent-question-blocker`, `thinking-block-validator`. The `stop-continuation-guard` was exported but not wired into `index.ts` — fixed.

**1.4 — Green CI in implement workflow.** Pre-ship verification step added to both Mode 1 (Fresh Implementation) and Mode 2 (Address Comments) requiring `bun test` + `bunx tsc --noEmit` + `bunx biome check` before proceeding. Worker records CI evidence in Linear comment.

### Phase 2: Cross-Family Review

**2.1 — Architect review step.** New "Cross-Family Review" section in `architect.md` between Act and Completion Signals. Spawns `review-architect` session with different model family. Evaluates acceptance criteria quality, completeness, testability. Skip when escalating.

**2.2 — Plan review step.** New cross-family review step in `plan.md` between writing-plans and Post to Linear. Spawns `review-plan` session. Quick reference table and workflow diagram updated.

**2.3 — Implementation review step.** New cross-family review step in `implement.md` Mode 1 between Pre-Ship Verification and Ship. Spawns `review-implementation` session. Mode 2 intentionally excluded (PR review handles it).

**2.4 — Multi-session team execution.** New "Parallel Execution with Task System" sub-section in `implement.md` Mode 1 under Invoke Skills. Explains: create task graph → spawn N sessions → each loops `task_claim_next` → execute → complete → convergence. Includes guidance on when to use parallel vs sequential.

### Phase 3: Pipeline Discipline

**3.1 — Human approval gate.** New `add_needs_approval` action in state machine (`packages/daemon/src/state/`). Flow: Backlog + worker-done → `needs-approval` label added, worker-done removed (pause). Backlog + needs-approval + human-approved → advance to Todo, dispatch planner. Manual Linear status changes intentionally bypass the gate (documented as expected behavior). 4 new tests.

**3.2 — Structured escalation.** Updated worker SKILL.md "Blocking on User Input" section with template: Phase, Completed, Blocker, Options Considered, Context (remaining estimate, expertise needed, branch).

**3.3 — Pre-transition quality gates.** New `PreCheckType = "quality-gate"` in state types. Set on `transition_to_needs_review` action only. Controller SKILL.md updated with "Quality Gate Check" section — trust-but-verify pattern: worker self-enforces, controller independently runs biome/tsc/test. 4 new tests.

**3.4 — Upfront parallelism annotation.** New "Parallelism Annotation" sub-step in `plan.md` after writing-plans. Tasks annotated as `Independent` or `Depends on: Task X, Task Y` for implementer task graph.

---

## Design Decisions Made During Implementation

1. **Permissions removed.** The plan called for tool-layer permission enforcement (deny edit/bash for reviewers). Two rounds of review found the OpenCode SDK doesn't support per-prompt permissions. Instead of building a complex workaround, we opted for prompt-based enforcement: review categories have system prompts saying "do NOT edit files." The only hard constraint is `LEAF_AGENTS` preventing delegation chains. Per-agent and per-category permission types/constants were all removed.

2. **`systemPrompt` uses `body.system` field.** Initially sent as a user text part. Corrected to use the SDK's `system` field in `promptAsync` for proper system-level instruction delivery.

3. **Env var naming.** `OPENCODE_TASK_LIST_ID` (primary) → `CLAUDE_CODE_TASK_LIST_ID` (fallback) → cwd basename. Follows OMO precedent of ecosystem compatibility.

4. **Approval gate bypass is intentional.** Manual Linear status changes (human drags issue from Backlog → Todo) bypass the approval gate. This is documented as expected behavior — humans who manually move issues are intentionally overriding the automated gate.

5. **Lock uses atomic rename, not unlink+create.** Stale lock reclaim writes a temp file, renames atomically over the stale lock, then verifies ownership. This eliminates the TOCTOU race where two processes could both unlink and recreate.

---

## Known Issues / Technical Debt

These are acknowledged but not fixed:

1. **Stale lock temp files accumulate on crash.** If a process crashes between `writeFileSync(temp)` and `renameSync`, the `.lock.{uuid}` temp file remains. Doesn't affect correctness (task file listing filters to `T-*.json`).

2. **Todo sync integration test gap.** Unit tests pass `ctx = undefined` so `syncTaskTodoUpdate` early-returns. The happy path with a real-ish client is untested.

3. **Lock can become stale while legitimately held.** Lock timestamp is written once and never refreshed. Any critical section >30s (including API calls like todo sync) becomes reclaimable by other processes.

4. **`todo-sync.ts` read-modify-write race.** Reads todos, modifies, writes back without optimistic concurrency. If the LLM's `todowrite` updates the list between read and write, sync overwrites LLM changes. Acceptable since todo sync is best-effort.

5. **`preCheck: "quality-gate"` consumer gap.** The state machine sets `preCheck` in the decision output. The controller SKILL.md describes how to read and act on it. But no TypeScript code reads it — it's consumed entirely by the skill markdown. If the controller skill fails to parse the JSON correctly, the gate is skipped silently.

---

## What Is NOT in Scope (Deferred per plan)

These are explicitly listed in the plan's "Deferred" section:

- **Continuation stack** (.sisyphus boulder/todo-continuation) — Revisit only if OpenCode native compaction + preemptive-compaction hook proves insufficient
- **Planning depth** (LEG-87 codebase index, LEG-67 impact analysis, LEG-88 proactive memory) — After Phases 0-2 are solid
- **Workflow adaptability** (LEG-90 dynamic orchestration, LEG-91 model-determined topology) — Make pipeline excellent before making it flexible
- **System intelligence** (context health monitoring, cost tracking, knowledge graph) — Exploratory
- **E2E testing capability** (LEG-69) — Deferred
- **Stuck detection** (LEG-89) — Deferred
- **2-pass reviews** (review-spec + review-quality per phase) — Future enhancement mentioned in plan
- **`task_heartbeat`** — Mentioned in plan for lease extension, not implemented (retry cap handles the common case)

---

## What Is NOT in Scope (Project Vision Boundaries)

These are architectural decisions about what Legion is and isn't:

- **Legion is NOT a second oh-my-opencode.** The plugin is a thin substrate. Workflow intelligence lives in skill markdown. Agent prompts are minimal — skills provide specialization via `/superpowers:*` and `/compound-engineering:*`.
- **No custom tool wrappers.** OpenCode provides native `grep`, `glob`, `lsp_*`, `ast_grep_*`, `interactive_bash`, `read`, `edit`, `write`, `skill`, `slashcommand`. The plugin doesn't wrap or duplicate these.
- **No behavioral hooks.** Hooks are only for capabilities skills genuinely can't provide (intercepting chat.params pipeline, session.error events, shell.env injection). No keyword detectors, auto-slash-commands, context monitors, etc.
- **Sessions, not jobs.** Every delegation is a full OpenCode session — observable, attachable, traceable via parentID. The `BackgroundTaskManager` is a session lifecycle manager, not an opaque job queue.
- **jj, not git.** Version control uses Jujutsu. No git commands. Workers operate in isolated jj workspaces.

---

## File Map for the Reviewer

### Plugin (packages/opencode-plugin/src/)
| Path | What it does |
|------|-------------|
| `index.ts` | Plugin entry point — registers agents, tools, hooks, config |
| `agents/` | 8 agent definitions (orchestrator, executor, oracle, explorer, librarian, metis, momus, multimodal) |
| `config/index.ts` | Config loading from `~/.config/opencode/opencode-legion.json` and `.opencode/opencode-legion.json` |
| `delegation/category-router.ts` | 11 categories with defaultModel + systemPrompt, `resolveCategory()` |
| `delegation/delegation-tool.ts` | `background_task`, `background_output`, `background_cancel` tools |
| `delegation/background-manager.ts` | Session lifecycle: launch (sync session create + async prompt), completion detection, cancel |
| `delegation/types.ts` | `BackgroundTask`, `LaunchOptions` interfaces |
| `hooks/` | 8 essential hooks (see 1.3 above) |
| `overlays/` | Model-specific system prompt overlays (claude, gemini, gpt) |
| `tools/session/` | Session management tools (list, read, info, search) |
| `tools/task/` | Dependency-aware task system (see 1.1 above) |

### Daemon (packages/daemon/src/)
| Path | What it does |
|------|-------------|
| `state/types.ts` | Issue status enum, label types, `ParsedIssue`, `IssueState`, `PreCheckType` |
| `state/decision.ts` | `buildIssueState()` — state machine for issue lifecycle transitions |
| `state/fetch.ts` | Parses Linear issue JSON into `ParsedIssue` |
| `state/cli.ts` | CLI entry point for state decisions (piped JSON → decision output) |
| `daemon/` | HTTP server, worker process management, port allocation |
| `cli/` | `legion start/stop/status/teams/attach` commands |

### Skills (.claude/skills/)
| Path | What it does |
|------|-------------|
| `legion-controller/SKILL.md` | Controller loop: fetch → decide → dispatch → sleep 30s |
| `legion-worker/SKILL.md` | Worker router: reads mode, delegates to workflow, escalation template |
| `legion-worker/workflows/architect.md` | Break down issues + cross-family review |
| `legion-worker/workflows/plan.md` | Create plans + parallelism annotation + cross-family review |
| `legion-worker/workflows/implement.md` | TDD coding + parallel execution + pre-ship CI + cross-family review + PR |
| `legion-worker/workflows/review.md` | PR review with line-level comments |
| `legion-worker/workflows/retro.md` | Retrospective → docs/solutions/ |
| `legion-worker/workflows/merge.md` | Merge PR, handle CI, cleanup |

---

## Review Instructions

Read the actual source code, not this summary. Be adversarial. The implementation was built by dispatching parallel subagents. Prior reviews focused on specific files — nobody has done a holistic read of the entire change as a coherent system.

**Key design principle:** The TS state machine is intended as a thin signal provider. It reports state and suggests actions, but the controller skill (an LLM reading markdown) makes the final decisions. Intelligence and policy should live in skills, not TypeScript. Evaluate whether the implementation actually achieves this, or whether logic has leaked into the wrong layer.

**Focus areas:**

1. **Does the system actually work end-to-end?** Trace a hypothetical issue through the full lifecycle: Triage → Backlog → architect (+ review) → needs-approval → human-approved → Todo → plan (+ review + parallelism annotation) → In Progress → implement (+ CI + cross-family review + quality gate) → Needs Review → review → Retro → Done. Does every handoff work? Are there dead paths? Can anything stall or loop?

2. **Skill ↔ TypeScript contract.** Skills invoke TypeScript via piped CLI and HTTP. TypeScript returns JSON that skills parse. Is the contract between them consistent? Does the controller skill correctly reference the fields in the state CLI output? Does the state CLI actually output the fields the skill expects?

3. **Task system under real concurrency.** Multiple workers claiming tasks simultaneously. Lease expiration during slow tasks. Crash recovery. The lock uses atomic rename for stale reclaim — is it actually correct under parallel workers? Walk through the claim flow with 3 concurrent workers.

4. **Cross-family review actually achievable?** The workflows say "use a different model family." But `background_task` takes a `model` override OR a `category`. The review categories have default models configured. If the implementer uses the same provider, is the "cross-family" promise actually enforced?

5. **Where does decision logic actually live?** The stated principle is "skills own intelligence, TS is a thin substrate." Check whether this is true. Is the state machine making policy decisions that should be controller decisions? Is the controller skill missing logic that it should own? Are there places where the state machine returning `skip` silently prevents the controller from acting?

6. **What's missing?** Compare the plan's requirements against the actual code. Look at failure modes: daemon restart mid-lifecycle, Linear API delays, worker crashes, GitHub API flakes. What happens in each case?
