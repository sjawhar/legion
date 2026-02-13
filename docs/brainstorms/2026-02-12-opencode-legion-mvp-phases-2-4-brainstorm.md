---
date: 2026-02-12
topic: opencode-legion-mvp-phases-2-4
---

# OpenCode-Legion Plugin: MVP Phases 2-4

## What We're Building

5 items to complete the opencode-legion plugin MVP. The original issue listed ~15 items across 3 phases, but brainstorming narrowed scope significantly:

- **Phase 3 (Agent Enforcement) dropped entirely** — skills handle planning constraints, verification reminders, and knowledge capture (via retro).
- **Phase 4 resilience hooks dropped** — OMO runs alongside the plugin and handles context-window-limit-recovery and edit-error-recovery.
- **Boulder state dropped** — replaced by Linear checklists (for Legion workers) and OpenCode's built-in todos + plan files (for interactive).
- **Start-work hook dropped** — already a skill.

What remains is the **continuation stack** (auto-continue, compaction resilience) and a **delegate-mode conductor agent**.

## Why This Approach

The plugin runs alongside OMO. Rather than reimplementing OMO's recovery hooks, we lean on OMO for those and focus on what's unique to our workflow:

1. **Custom continuation** — OMO's continuation is boulder-centric. Ours works with OpenCode's built-in todos and plan file workflows (brainstorm -> plan -> implement -> review).
2. **Delegate-mode** — new capability not in OMO. Forces orchestration through delegation only.
3. **Minimal viable** — simplest implementation that works. Add sophistication later.

## Key Decisions

- **No boulder.json**: Progress tracking for Legion workers goes through Linear issue checklists. Interactive sessions use OpenCode's built-in todos + plan files.
- **Continuation applies to orchestrator agents only**: orchestrator, executor, builder auto-continue. Leaf agents (explorer, librarian, oracle, metis, momus, multimodal) don't.
- **Minimal continuation prompt**: Just "continue working" — no todo list serialization. The model has session context already.
- **OMO coexistence**: Recovery hooks (context-window-limit, edit-error) delegated to OMO. We own continuation and delegation behavior.
- **Conductor is a new agent, not a mode**: Clean separation. Users explicitly choose the conductor agent when they want delegation-only.

## Items

### 1. todo-continuation-enforcer (~100 LOC)

Auto-inject continuation prompt when session idles with incomplete todos.

**Hook**: `event` (listens for `session.idle`)

**Logic**:
1. On `session.idle`, extract sessionID
2. Check stop-continuation flag via `stopContinuationGuard.isStopped(sessionID)` — bail if stopped
3. Resolve current agent from session — bail if not in continuation agents set (`orchestrator`, `executor`, `builder`)
4. Fetch todos via `ctx.client.session.todo({ path: { id: sessionID } })`
5. Count incomplete (status !== "completed" and status !== "cancelled")
6. If no incomplete todos — bail (work is done)
7. Start 2-second grace timer. Cancel if user sends a message (track via `chat.message` hook)
8. After grace period, inject continuation via `ctx.client.session.promptAsync({ path: { id: sessionID }, body: { content: CONTINUATION_PROMPT } })`

**Continuation prompt**:
```
Continue working on the next incomplete task. Pick up where you left off — do not re-read the plan or re-summarize progress.
```

**State** (in-memory):
```typescript
const pendingTimers = new Map<string, Timer>(); // sessionID -> grace period timer
const CONTINUATION_AGENTS = new Set(["orchestrator", "executor", "builder"]);
const GRACE_PERIOD_MS = 2000;
```

**Integration with session-recovery**: If session-recovery is handling an error, skip continuation. Track via a `recovering` Set<sessionID> that session-recovery populates.

**Wiring in index.ts**:
- `event` handler: listen for `session.idle`, `session.deleted` (cleanup timer)
- `chat.message` handler: cancel pending timer on user message

---

### 2. compaction-todo-preserver (~80 LOC)

Save/restore OpenCode todos across session compaction.

**Hooks**: `experimental.session.compacting` (pre-compaction capture) + `event` (post-compaction restore)

**Logic**:
1. **Capture** (called from `experimental.session.compacting`): Fetch current todos, store in `Map<sessionID, Todo[]>`
2. **Restore** (called on `session.compacted` event): Check if todos exist post-compaction. If lost, restore from snapshot.
3. **Cleanup** (on `session.deleted`): Delete snapshot.

**State**:
```typescript
const snapshots = new Map<string, TodoSnapshot[]>();
```

**Integration**: The preemptive-compaction hook triggers `experimental.session.compacting`. We call `capture()` there before compaction proceeds.

---

### 3. compaction-context-injector (~60 LOC)

Inject structured context template into compaction summaries so the model retains what it was doing.

**Hook**: `experimental.session.compacting`

**Logic**: Push a template string into `output.context[]`. The template instructs the compaction summarizer to preserve:

1. User requests (verbatim)
2. Final goal
3. Work completed (files modified, features implemented)
4. Remaining tasks
5. Active working context (files being edited, code in progress, external references)
6. Explicit constraints (verbatim only, no invention)

**Template** (simplified from OMO's 7 sections to 6 — dropped "Agent Verification State" since our skills handle reviewer continuity):

```typescript
const COMPACTION_CONTEXT_TEMPLATE = `
When summarizing this session, include these sections:

## 1. User Requests (As-Is)
List all original user requests exactly as stated. Preserve exact wording.

## 2. Final Goal
What the user ultimately wanted to achieve.

## 3. Work Completed
What has been done: files created/modified, features implemented, problems solved.

## 4. Remaining Tasks
What still needs to be done. Pending items, follow-up tasks identified.

## 5. Active Working Context
- Files being edited or frequently referenced
- Code in progress (key snippets, function signatures)
- External references (docs, APIs, URLs)
- Important state (variable names, config values)

## 6. Explicit Constraints (Verbatim Only)
Only constraints explicitly stated by the user. Quote verbatim. If none, write "None".
`;
```

---

### 4. Wire stop-continuation (~15 LOC)

The `stop-continuation-guard` hook already exists and is wired into `event` and `chat.message`. The stop-continuation command template needs to be created so users can invoke `/stop-continuation`.

**What's needed**:
- Create `stop-continuation` as a builtin command template that calls `stopContinuationGuard.stop(sessionID)`
- Verify the existing guard's `isStopped()` method is consumed by the todo-continuation-enforcer (Item 1)

This is mostly wiring — connecting the existing pieces.

---

### 5. Conductor agent (delegate-mode) (~150 LOC)

A new first-class agent that can only read, search, and delegate. Cannot directly edit code, write files, or run bash commands.

**Agent definition**:
```typescript
{
  name: "conductor",
  description: "Orchestrates work exclusively through delegation. Cannot modify code directly.",
  config: {
    model: "anthropic/claude-sonnet-4-20250514",
    temperature: 0.7,
    prompt: CONDUCTOR_PROMPT,
  },
  permission: {
    read: "allow",
    glob: "allow",
    list: "allow",
    edit: "deny",
    bash: "deny",
    task: "allow",
  }
}
```

**Prompt** (key additions vs orchestrator):
- Identity: "You are a conductor. You coordinate work by delegating to specialists."
- Constraint: "You MUST NOT modify code, files, or run shell commands directly. Your only tools for making changes are delegation via background_task."
- Workflow: "For each task: (1) understand the requirement, (2) break into delegatable units, (3) dispatch to appropriate specialist, (4) verify results, (5) report back."
- Allowed direct actions: reading files, searching codebase, analyzing code, planning, communicating with user.

**Permission enforcement**: The `permission.edit: "deny"` and `permission.bash: "deny"` settings are enforced by OpenCode's permission system at the platform level. The prompt reinforces this behaviorally.

## Dependency Order

```
stop-continuation-guard (exists) ──┐
                                   ├── todo-continuation-enforcer
session-recovery (exists) ─────────┘
                                   
preemptive-compaction (exists) ──── compaction-todo-preserver
                                   
(standalone) ────────────────────── compaction-context-injector

(standalone) ────────────────────── wire stop-continuation command

(standalone) ────────────────────── conductor agent
```

Items 2-5 are independent of each other. Item 1 depends on the stop-continuation-guard (exists) and session-recovery (exists) for integration.

## Implementation Order

1. **compaction-context-injector** — standalone, ~60 LOC, zero dependencies
2. **compaction-todo-preserver** — standalone, ~80 LOC, integrates with preemptive-compaction
3. **wire stop-continuation** — ~15 LOC, completes existing work
4. **todo-continuation-enforcer** — ~100 LOC, depends on stop-continuation-guard
5. **conductor agent** — ~150 LOC, standalone

Total estimated: **~400 LOC** (vs. OMO's ~3,500 LOC for equivalent functionality)

## Resolved Questions

- **Continuation agent resolution**: Walk `ctx.client.session.messages()` backwards. Each message has `info.agent`, `info.model`, `info.providerID`, `info.modelID`. Find the first non-compaction message with agent info. Pass resolved `agent` and `model` to `promptAsync()` so continuation resumes on the same agent/model.
- **OMO hook conflict**: OMO's `todo-continuation-enforcer` is already disabled in `~/.config/opencode/oh-my-opencode.json` (`disabled_hooks` list). Also disabled: `preemptive-compaction`, `start-work`, `atlas`, `prometheus-md-only`, `sisyphus-junior-notepad`, `agent-usage-reminder`, `category-skill-reminder`. No conflict.
- **Conductor model**: Configurable via the existing agent config system (same as all agents). Not a design decision.

## Next Steps

-> Plan implementation details for each item
