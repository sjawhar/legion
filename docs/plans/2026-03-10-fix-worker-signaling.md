# Worker Signaling Reliability Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ensure workers reliably complete end-of-work signaling (`worker-done` label, push/PR/comment steps) even when long runs approach context limits.

**Architecture:** Implement three defensive layers: (1) plugin-level idle-time signaling enforcement, (2) workflow-level signaling todo hardening so continuation stays active, and (3) controller-level stalled-worker fallback that resumes signaling or performs safe terminal labeling when work is already verifiably complete. The design treats context exhaustion as expected behavior and moves final signaling guarantees to increasingly external control loops.

**Tech Stack:** TypeScript, Bun, OpenCode plugin hooks (`event`, `chat.message`, `tool.execute.before`, `experimental.*`), Legion worker/controller skills (Markdown workflows), GitHub CLI, daemon state machine.

---

## Hook Point Evaluation (Required Design Inputs)

The OpenCode plugin hook surface (from `@opencode-ai/plugin/dist/index.d.ts`) currently supports:

- `event`
- `config`
- `tool`
- `auth`
- `chat.message`
- `chat.params`
- `chat.headers`
- `permission.ask`
- `command.execute.before`
- `tool.execute.before`
- `tool.execute.after`
- `shell.env`
- `experimental.chat.messages.transform`
- `experimental.chat.system.transform`
- `experimental.session.compacting`
- `experimental.text.complete`

Relevant event types (from `@opencode-ai/sdk`): `session.status`, `session.idle`, `session.error`, `session.compacted`, `session.deleted`, etc.

**Conclusion for issue #85:**
- `session.idle` already exists and is the correct hook for post-work signaling injection.
- There is no `agent.turn.complete` hook in current plugin API.
- We should not block on adding new core hook APIs; implement with existing `event` + `session.idle` now.

---

### Task 1: Add Plugin-Level Worker Signaling Enforcer

**Files:**
- Create: `packages/opencode-plugin/src/hooks/worker-signaling-enforcer.ts`
- Modify: `packages/opencode-plugin/src/index.ts`
- Test: `packages/opencode-plugin/src/__tests__/integration.test.ts`

**Step 1: Write failing tests for idle-time signaling injection**

Add integration tests that simulate:
- Worker session idles with unfinished signaling context -> signaling prompt is injected.
- Session idles but signaling is already complete -> no injection.
- Background session -> no injection.
- Stop-continuation active -> no injection.
- Prompt injection error -> swallowed with warning (no crash).

Use existing continuation test style in `integration.test.ts` as a template (stub `session.todo`, `session.messages`, `session.promptAsync`).

**Step 2: Run targeted test to confirm RED state**

Run: `bun test src/__tests__/integration.test.ts`
Expected: New signaling-enforcer tests fail because hook does not exist yet.

**Step 3: Implement `worker-signaling-enforcer` hook**

Implement a new hook that listens on `event` for `session.idle` and performs guarded injection:

1. Resolve session ID from event props.
2. Bail if session is background, recovering, or continuation is manually stopped.
3. Detect worker context (session appears to be a Legion worker flow, not random interactive session).
4. Detect whether signaling is still pending (initial version should use explicit todo marker; see Task 2).
5. If pending, inject a concise signaling-only prompt via `ctx.client.session.promptAsync`.

Prompt text should be narrow and non-ambiguous, e.g.:

```text
Complete only workflow signaling now: finish exit/signaling steps (label updates, push/PR/comment checks) and then stop.
```

**Step 4: Wire the hook in plugin entrypoint**

In `packages/opencode-plugin/src/index.ts`:
- Initialize the new hook with existing guard dependencies used by `todo-continuation-enforcer`.
- Call its `event` handler in the main `event` pipeline.
- Call its `chat.message` cancellation path if implemented (mirrors timer invalidation strategy).

**Step 5: Run test to confirm GREEN state**

Run: `bun test src/__tests__/integration.test.ts`
Expected: Signaling-enforcer tests and existing continuation tests pass.

**Step 6: Commit checkpoint**

```bash
jj describe -m "fix(plugin): inject signaling prompt when worker idles before completion"
jj new
```

---

### Task 2: Harden Worker Workflows with Early Signaling Todo

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/implement.md`
- Modify: `.opencode/skills/legion-worker/workflows/plan.md`
- Modify: `.opencode/skills/legion-worker/workflows/test.md`
- Modify: `.opencode/skills/legion-worker/workflows/review.md`
- Modify: `.opencode/skills/legion-worker/workflows/architect.md`
- (No change expected) `.opencode/skills/legion-worker/workflows/merge.md`

**Step 1: Add explicit signaling todo at workflow start**

In each affected workflow, add an early step that creates a persistent todo such as:

- `Signal completion (worker-done + worker-active cleanup + required PR/CI/comment checks)`

and mark it as `in_progress` early. Keep it incomplete until final signaling step.

**Step 2: Mark signaling todo complete only at final signaling step**

At current final exit/signaling sections, add explicit instruction to mark the signaling todo `completed` only after label application verification succeeds.

**Step 3: Keep merge workflow unchanged**

Document in `merge.md` (or in a shared note referenced by merge) that `worker-done` is intentionally not used for merge mode.

**Step 4: Validate markdown consistency**

Run: `bun test src/__tests__/integration.test.ts`
Expected: No plugin regression from workflow wording changes (tests remain green).

**Step 5: Commit checkpoint**

```bash
jj describe -m "fix(workflows): create signaling todo early to keep continuation alive"
jj new
```

---

### Task 3: Extend Plugin Signaling Detection to Use Todo Semantics

**Files:**
- Modify: `packages/opencode-plugin/src/hooks/worker-signaling-enforcer.ts`
- Test: `packages/opencode-plugin/src/__tests__/integration.test.ts`

**Step 1: Add deterministic signaling-todo matcher**

Implement matcher logic to identify signaling todo items by stable phrase/prefix (exact convention added in Task 2).

**Step 2: Enforce "inject only when signaling todo incomplete"**

Before injecting prompt, fetch todos and require at least one incomplete signaling todo.

**Step 3: Add tests for matcher behavior**

Add tests:
- Incomplete signaling todo -> inject.
- Signaling todo completed -> do not inject.
- Non-signaling todos only -> do not inject.

**Step 4: Run tests**

Run: `bun test src/__tests__/integration.test.ts`
Expected: All pass.

**Step 5: Commit checkpoint**

```bash
jj describe -m "fix(plugin): gate idle signaling prompts on dedicated signaling todo"
jj new
```

---

### Task 4: Add Controller Fallback for Stalled Signaling

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`
- Modify: `packages/daemon/src/state/decision.ts` (if new explicit action is added)
- Modify: `packages/daemon/src/state/types.ts` (if new action enum value is added)
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts`
- Modify: `packages/daemon/src/state/__tests__/decision-regressions.test.ts`

**Step 1: Define stalled-signaling policy**

Policy condition (GitHub backend):
- Issue is in active worker pipeline status (`In Progress`, `Testing`, `Needs Review`, or `Retro`).
- No live worker response progression for configurable threshold (e.g. >10 minutes idle/dead/no session progress).
- Missing `worker-done`.

**Step 2: Implement fallback behavior in controller skill**

In controller loop logic:
1. First fallback: resume existing worker with signaling-only prompt.
2. Second fallback (ultimate, external): if objective evidence shows work is already complete (PR exists, branch pushed, CI status acceptable for stage), apply labels directly (`worker-done`, remove `worker-active`) and post an audit comment describing automated fallback.

Use daemon endpoints (`/workers`, `/workers/:id/status`) and existing state signals; do not inspect serve internals directly.

**Step 3: Optionally codify as explicit state action**

If controller logic would be cleaner with a first-class state action, add one (example: `resume_worker_for_signaling`) through state types + decision mapping + tests.

**Step 4: Add/adjust state decision tests**

Cover:
- Existing behavior remains unchanged for normal paths.
- New fallback action (if introduced) only appears under stalled-signaling conditions.

**Step 5: Run daemon state tests**

Run:
- `bun test src/state/__tests__/decision.test.ts src/state/__tests__/decision-regressions.test.ts`

Expected: Pass.

**Step 6: Commit checkpoint**

```bash
jj describe -m "fix(controller): recover stalled workers that miss final signaling"
jj new
```

---

### Task 5: Handle True Context Exhaustion Explicitly (No More Injection Possible)

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`
- Modify: `.opencode/skills/legion-worker/workflows/implement.md`
- Modify: `.opencode/skills/legion-worker/workflows/test.md`
- Modify: `.opencode/skills/legion-worker/workflows/review.md`
- Modify: `.opencode/skills/legion-worker/workflows/plan.md`
- Modify: `.opencode/skills/legion-worker/workflows/architect.md`

**Step 1: Document hard-failure path in controller**

Add explicit "when prompt injection/resume fails repeatedly" section:
- Retry limited number of times.
- Then perform controller-side terminal signaling only when objective completion evidence exists.
- Otherwise mark as `user-input-needed` with precise diagnostics.

**Step 2: Add worker-side breadcrumbs to aid fallback**

In workflow docs, require workers to post/maintain minimal verifiable breadcrumbs before heavy operations where possible (e.g., branch name/PR number in comments when created). This improves controller confidence for safe fallback labeling.

**Step 3: Ensure fallback is backend-aware**

Retain GitHub + Linear branches in policy text, but treat GitHub as first implementation target (issue #85 is GitHub-driven).

**Step 4: Commit checkpoint**

```bash
jj describe -m "docs(controller): codify ultimate fallback when session cannot be resumed"
jj new
```

---

### Task 6: End-to-End Validation and Regression Sweep

**Files:**
- Test: `packages/opencode-plugin/src/__tests__/integration.test.ts`
- Test: `packages/daemon/src/state/__tests__/decision.test.ts`
- Test: `packages/daemon/src/state/__tests__/decision-regressions.test.ts`

**Step 1: Run plugin test suite**

Run: `bun test src/__tests__/integration.test.ts`
Expected: Pass.

**Step 2: Run daemon state tests**

Run: `bun test src/state/__tests__/decision.test.ts src/state/__tests__/decision-regressions.test.ts`
Expected: Pass.

**Step 3: Run project checks for touched packages**

Run:
- `bunx tsc --noEmit`
- `bunx biome check packages/opencode-plugin/src packages/daemon/src/state`

Expected: No type or lint errors.

**Step 4: Manual scenario verification checklist (required before ship)**

Validate these scenarios in a controlled test issue:
1. Normal completion path still signals once (no duplicate prompts).
2. Idle before signaling triggers plugin injection and completes signaling.
3. Simulated hard-stop worker gets recovered by controller fallback.
4. Merge mode is unaffected (no `worker-done` required).

**Step 5: Final commit checkpoint**

```bash
jj describe -m "fix: harden worker completion signaling across plugin workflow and controller"
jj new
```

---

## Testing Plan

### Setup
- `bun install`
- Plugin package tests run from `packages/opencode-plugin`
- Daemon package tests run from `packages/daemon`

### Health Check
- `bun --version` returns successfully.
- `gh auth status` works for GitHub-backed integration checks.

### Verification Steps
1. **Plugin idle injection behavior**
   - Action: Run plugin integration tests with new signaling cases.
   - Expected: Injection happens only for incomplete signaling todos in worker sessions.
   - Tool: Bun test.

2. **State/controller fallback routing**
   - Action: Run daemon decision tests.
   - Expected: Existing transitions unchanged; stalled-signaling fallback path covered.
   - Tool: Bun test.

3. **Workflow hardening semantics**
   - Action: Inspect workflow markdown changes.
   - Expected: Each non-merge worker mode creates signaling todo early and completes it only at exit signaling.
   - Tool: Manual review + grep.

4. **No regression in core quality gates**
   - Action: Run typecheck + biome on changed paths.
   - Expected: Clean.
   - Tool: Bunx.

### Tools Needed
- Bun (`bun test`, `bunx tsc`, `bunx biome`)
- GitHub CLI (`gh`) for real signaling flow checks
- Read/Grep for workflow and skill policy verification

---

## Risk Notes

- Primary risk is false-positive signaling injection in non-worker sessions; mitigated by strict worker-context detection + signaling-todo gating.
- Controller-side direct labeling must be constrained to evidence-backed cases only; otherwise it can advance incomplete work.
- Do not rely on adding new OpenCode core hook types for this fix; implement with current hook API.
