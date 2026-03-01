# State Machine Hardening Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix 8 pre-existing issues (P1 + P2) in the state machine decision logic and worker skill files, surfaced during the testing gate review (#63).

**Architecture:** Pure logic changes in `decision.ts` (no I/O), type additions in `types.ts`, and markdown fixes in workflow skills. The state machine is a pure function: `(status, signals) → action`. Changes are isolated to the decision layer and skill documentation. No daemon, fetch, or server changes.

**Tech Stack:** TypeScript/Bun, Biome linter, bun:test, jj version control

---

## Key Findings from Analysis

1. **Item 13 is partially incorrect:** The retro skill (`legion-retro/SKILL.md` step 7) already adds `worker-done` after retro completion. The actual bug is `resume_implementer_for_retro` spamming every controller loop.
2. **Item 14/18 share a root cause:** No explicit lifecycle transition to Done. The merge workflow should explicitly close the issue rather than relying on GitHub auto-close.
3. **Item 17 is partially incorrect:** All workflow completion paths DO include `worker-active` removal. The gap is in blocking paths (`user-input-needed`) and the merge workflow's exit-without-labels path.
4. **Items 11, 12, 13, 15 all modify `decision.ts`** — these must be sequential (same file).

---

## Task 1: Fix In Progress deadlock recovery (Item 11, P1)

**Files:**
- Modify: `packages/daemon/src/state/decision.ts:58-60`
- Modify: `packages/daemon/src/state/__tests__/decision-regressions.test.ts:6-27`
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts` (add new test)

**Depends on:** Nothing — Independent

**Step 1: Write the failing test**

Add to `packages/daemon/src/state/__tests__/decision.test.ts` in the `suggestAction` describe block:

```typescript
it("in_progress_has_pr_no_worker_done_no_live_worker_dispatches_implementer", () => {
  const action = suggestAction(IssueStatus.IN_PROGRESS, false, false, null, true, false);
  expect(action).toBe("dispatch_implementer");
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: FAIL — currently returns `"skip"` not `"dispatch_implementer"`

**Step 3: Update the existing regression test**

In `decision-regressions.test.ts`, update the first test to expect the new behavior:

```typescript
it("dispatches implementer when In Progress issue has PR but no live worker and no worker-done", () => {
  const data: FetchedIssueData = {
    issueId: "ENG-21",
    status: "In Progress",
    labels: [],
    hasPr: true,
    prIsDraft: null,
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    hasUserFeedback: false,
    hasUserInputNeeded: false,
    hasNeedsApproval: false,
    hasHumanApproved: false,
    hasTestPassed: false,
    hasTestFailed: false,
    source: null,
  };

  const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
  expect(state.suggestedAction).toBe("dispatch_implementer");
});
```

**Step 4: Remove the existing `in_progress_has_pr_no_worker_done_no_live_worker_skips` test**

In `decision.test.ts`, find and remove:

```typescript
it("in_progress_has_pr_no_worker_done_no_live_worker_skips", () => {
  const action = suggestAction(IssueStatus.IN_PROGRESS, false, false, null, true, false);
  expect(action).toBe("skip");
});
```

**Step 5: Fix the decision logic**

In `packages/daemon/src/state/decision.ts`, in the `IN_PROGRESS` case, remove the `hasPr && !hasLiveWorker` early return:

```typescript
case IssueStatus.IN_PROGRESS:
  if (hasWorkerDone) {
    return "transition_to_testing";
  }
  if (hasLiveWorker) {
    return "skip";
  }
  return "dispatch_implementer";
```

This removes lines 58-60 (`if (hasPr && !hasLiveWorker) { return "skip"; }`). Now if the worker dies after opening a PR but before adding `worker-done`, a fresh implementer is dispatched to resume work.

**Step 6: Run tests to verify everything passes**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts packages/daemon/src/state/__tests__/decision-regressions.test.ts`
Expected: All PASS

**Step 7: Describe and advance**

```bash
jj describe -m "fix: resolve In Progress deadlock when worker dies after PR creation (#65)"
jj new
```

---

## Task 2: Scope `needs-approval` to Backlog/Todo only (Item 12, P2)

**Files:**
- Modify: `packages/daemon/src/state/decision.ts:143-146`
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts` (add new tests)

**Depends on:** Task 1 (same file)

**Step 1: Write the failing test**

Add to `decision.test.ts` in the `approval gate` describe block:

```typescript
it("needs_approval_on_non_backlog_status_follows_normal_flow", () => {
  const data: FetchedIssueData = {
    issueId: "ENG-21",
    status: "In Progress",
    labels: ["needs-approval", "worker-done"],
    hasPr: false,
    prIsDraft: null,
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    hasUserFeedback: false,
    hasUserInputNeeded: false,
    hasNeedsApproval: true,
    hasHumanApproved: false,
    hasTestPassed: false,
    hasTestFailed: false,
    source: null,
  };

  const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
  // Should follow normal In Progress flow (worker-done → transition_to_testing)
  // NOT be frozen by leaked needs-approval label
  expect(state.suggestedAction).toBe("transition_to_testing");
});

it("needs_approval_on_todo_still_works", () => {
  const data: FetchedIssueData = {
    issueId: "ENG-21",
    status: "Todo",
    labels: ["needs-approval"],
    hasPr: false,
    prIsDraft: null,
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    hasUserFeedback: false,
    hasUserInputNeeded: false,
    hasNeedsApproval: true,
    hasHumanApproved: false,
    hasTestPassed: false,
    hasTestFailed: false,
    source: null,
  };

  const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
  expect(state.suggestedAction).toBe("skip");
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: First test FAILS (currently returns `"skip"` due to global freeze)

**Step 3: Fix the decision logic**

In `packages/daemon/src/state/decision.ts`, update `buildIssueState` to scope `needs-approval` checks:

```typescript
} else if (
  data.hasNeedsApproval &&
  data.hasHumanApproved &&
  (data.status === IssueStatus.BACKLOG || data.status === IssueStatus.TODO)
) {
  action = "transition_to_todo";
} else if (
  data.hasNeedsApproval &&
  (data.status === IssueStatus.BACKLOG || data.status === IssueStatus.TODO)
) {
  action = "skip";
} else {
```

**Step 4: Run tests to verify everything passes**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: All PASS

**Step 5: Describe and advance**

```bash
jj describe -m "fix: scope needs-approval check to Backlog/Todo statuses only (#65)"
jj new
```

---

## Task 3: Fix Retro live worker spam (Item 13, P2)

**Files:**
- Modify: `packages/daemon/src/state/decision.ts:100-102`
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts` (update existing test)
- Modify: `packages/daemon/src/state/__tests__/decision-regressions.test.ts` (update existing test)

**Depends on:** Task 2 (same file)

**Step 1: Write the failing test**

Update the existing test in `decision.test.ts`:

```typescript
it("retro_with_live_worker_skips", () => {
  const action = suggestAction(IssueStatus.RETRO, false, true, null, false, false);
  expect(action).toBe("skip");
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: FAIL — currently returns `"resume_implementer_for_retro"`

**Step 3: Update the regression test**

In `decision-regressions.test.ts`, update:

```typescript
it("skips Retro when a live worker exists (worker is already running retro)", () => {
  const data: FetchedIssueData = {
    issueId: "ENG-22",
    status: "Retro",
    labels: [],
    hasPr: true,
    prIsDraft: false,
    hasLiveWorker: true,
    workerMode: null,
    workerStatus: null,
    hasUserFeedback: false,
    hasUserInputNeeded: false,
    hasNeedsApproval: false,
    hasHumanApproved: false,
    hasTestPassed: false,
    hasTestFailed: false,
    source: null,
  };

  const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
  expect(state.suggestedAction).toBe("skip");
});
```

**Step 4: Fix the decision logic**

In `packages/daemon/src/state/decision.ts`, change the Retro case:

```typescript
case IssueStatus.RETRO:
  if (hasWorkerDone) {
    return "dispatch_merger";
  }
  if (hasLiveWorker) {
    return "skip";
  }
  return "dispatch_implementer_for_retro";
```

This aligns Retro with all other statuses: live worker → skip. The retro worker is dispatched once via `dispatch_implementer_for_retro` and runs without being spammed.

Note: `resume_implementer_for_retro` remains in `ActionType` and `ACTION_TO_MODE` as dead code — removing it would change the type union and require updates in any code referencing ActionType. Can be cleaned up in a follow-up.

**Step 5: Run tests to verify everything passes**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts packages/daemon/src/state/__tests__/decision-regressions.test.ts`
Expected: All PASS

**Step 6: Describe and advance**

```bash
jj describe -m "fix: prevent resume_implementer_for_retro from spamming every controller loop (#65)"
jj new
```

---

## Task 4: Fix sessionId for skip actions (Item 15, P2)

**Files:**
- Modify: `packages/daemon/src/state/decision.ts:161`
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts` (add new test)

**Depends on:** Task 3 (same file)

**Step 1: Write the failing test**

Add to `decision.test.ts` in the `buildIssueState` describe block:

```typescript
it("skip_with_live_worker_uses_actual_worker_mode_for_session_id", () => {
  const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
  const data: FetchedIssueData = {
    issueId: "ENG-21",
    status: "Testing",
    labels: [],
    hasPr: true,
    prIsDraft: null,
    hasLiveWorker: true,
    workerMode: "test",
    workerStatus: null,
    hasUserFeedback: false,
    hasUserInputNeeded: false,
    hasNeedsApproval: false,
    hasHumanApproved: false,
    hasTestPassed: false,
    hasTestFailed: false,
    source: null,
  };

  const state = buildIssueState(data, teamId);
  expect(state.suggestedAction).toBe("skip");
  // sessionId should use the tester's mode, not the default implement mode
  const expectedSessionId = computeSessionId(teamId, "ENG-21", "test");
  expect(state.sessionId).toBe(expectedSessionId);
});

it("skip_without_worker_mode_falls_back_to_action_to_mode", () => {
  const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
  const data: FetchedIssueData = {
    issueId: "ENG-21",
    status: "Done",
    labels: [],
    hasPr: false,
    prIsDraft: null,
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    hasUserFeedback: false,
    hasUserInputNeeded: false,
    hasNeedsApproval: false,
    hasHumanApproved: false,
    hasTestPassed: false,
    hasTestFailed: false,
    source: null,
  };

  const state = buildIssueState(data, teamId);
  expect(state.suggestedAction).toBe("skip");
  // No workerMode → falls back to ACTION_TO_MODE["skip"] = implement
  const expectedSessionId = computeSessionId(teamId, "ENG-21", "implement");
  expect(state.sessionId).toBe(expectedSessionId);
});
```

**Step 2: Run test to verify first test fails**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: First test FAILS — sessionId uses implement mode instead of test mode

**Step 3: Fix the sessionId computation**

In `packages/daemon/src/state/decision.ts`, replace the mode + sessionId computation in `buildIssueState`:

```typescript
const VALID_WORKER_MODES = new Set<string>([
  WorkerMode.ARCHITECT,
  WorkerMode.PLAN,
  WorkerMode.IMPLEMENT,
  WorkerMode.TEST,
  WorkerMode.REVIEW,
  WorkerMode.MERGE,
]);

// Use actual worker mode for skip actions when available
let mode: WorkerModeLiteral;
if (action === "skip" && data.workerMode && VALID_WORKER_MODES.has(data.workerMode)) {
  mode = data.workerMode as WorkerModeLiteral;
} else {
  mode = ACTION_TO_MODE[action] ?? WorkerMode.IMPLEMENT;
}
const sessionId = computeSessionId(teamId, data.issueId, mode);
```

Place the `VALID_WORKER_MODES` set at module scope (after `ACTION_TO_MODE`), not inside the function.

**Step 4: Run tests to verify everything passes**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts packages/daemon/src/state/__tests__/decision-regressions.test.ts`
Expected: All PASS

**Step 5: Describe and advance**

```bash
jj describe -m "fix: use actual worker mode for sessionId in skip actions (#65)"
jj new
```

---

## Task 5: Add `transition_to_done` and fix merge workflow (Items 14, 18, P2)

**Files:**
- Modify: `packages/daemon/src/state/types.ts:41-62` (ActionType union)
- Modify: `packages/daemon/src/state/decision.ts:110-132` (ACTION_TO_MODE)
- Modify: `.opencode/skills/legion-worker/workflows/merge.md:79-81`
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts` (add test)

**Depends on:** Task 4 (types.ts and decision.ts shared)

**Step 1: Add `transition_to_done` to ActionType**

In `packages/daemon/src/state/types.ts`, add to the ActionType union:

```typescript
export type ActionType =
  | "skip"
  | "investigate_no_pr"
  | "dispatch_architect"
  | "dispatch_planner"
  | "dispatch_implementer"
  | "dispatch_implementer_for_retro"
  | "dispatch_tester"
  | "transition_to_testing"
  | "resume_implementer_for_test_failure"
  | "dispatch_reviewer"
  | "dispatch_merger"
  | "resume_implementer_for_changes"
  | "resume_implementer_for_retro"
  | "transition_to_in_progress"
  | "transition_to_needs_review"
  | "transition_to_retro"
  | "transition_to_todo"
  | "transition_to_done"
  | "relay_user_feedback"
  | "remove_worker_active_and_redispatch"
  | "add_needs_approval"
  | "retry_pr_check";
```

**Step 2: Add to ACTION_TO_MODE**

In `packages/daemon/src/state/decision.ts`, add to `ACTION_TO_MODE`:

```typescript
transition_to_done: WorkerMode.MERGE,
```

**Step 3: Write test for transition_to_done in ACTION_TO_MODE**

Add to `decision.test.ts`:

```typescript
it("transition_to_done_is_in_action_to_mode", () => {
  expect("transition_to_done" in ACTION_TO_MODE).toBe(true);
  expect(ACTION_TO_MODE["transition_to_done"]).toBe("merge");
});
```

**Step 4: Update merge workflow to explicitly close issue**

In `.opencode/skills/legion-worker/workflows/merge.md`, replace step 7:

```markdown
### 7. Close Issue

After successful merge, explicitly close the issue to transition to Done:

**GitHub:**
```bash
gh issue close $ISSUE_NUMBER -R $OWNER/$REPO --comment "Closed via PR merge."
```

**Linear:**
```
linear_linear(action="update", id=$LEGION_ISSUE_ID, state="Done")
```

Then remove `worker-active` if present:
- **GitHub:** `gh issue edit $ISSUE_NUMBER --remove-label "worker-active" -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current labels without "worker-active"])`

> **Note:** GitHub auto-close may also fire when the PR merges, which is fine — closing an already-closed issue is a no-op. This explicit close is a safety net for cases where auto-close doesn't trigger (e.g., issue not linked to PR, Linear backend).
```

**Step 5: Run tests**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: All PASS

**Step 6: Describe and advance**

```bash
jj describe -m "feat: add transition_to_done action and explicit issue close in merge workflow (#65)"
jj new
```

---

## Task 6: Document undocumented tools in implement workflow (Item 16, P2)

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/implement.md`

**Depends on:** Nothing — Independent

**Step 1: Add tools documentation section**

In `.opencode/skills/legion-worker/workflows/implement.md`, add after the "Mode Detection" section (before "All Modes: Rebase First"):

```markdown
## Tools Referenced

This workflow references environment-provided tools. These are available in the OpenCode runtime, not defined in this repo:

| Tool | Source | Purpose |
|------|--------|---------|
| `task_create` | OpenCode task system | Create a task with dependencies (`blockedBy`) |
| `task_claim_next` | OpenCode task system | Atomically claim the next ready task |
| `task_update` | OpenCode task system | Mark task completed/failed |
| `task_list` | OpenCode task system | List tasks and their status |
| `background_task` | OpenCode agent system | Spawn a background subagent |
| `/analyze` | `sjawhar/analyze` skill | Run code quality agents on recent changes |

The task system enables parallel execution with dependency ordering. If these tools are unavailable in your environment, execute tasks sequentially following the plan's dependency annotations.
```

**Step 2: Describe and advance**

```bash
jj describe -m "docs: document external tools referenced in implement workflow (#65)"
jj new
```

---

## Task 7: Fix `worker-active` cleanup in all exit paths (Item 17, P2)

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/architect.md`
- Modify: `.opencode/skills/legion-worker/workflows/plan.md`
- Modify: `.opencode/skills/legion-worker/workflows/review.md`

**Depends on:** Nothing — Independent

**Context:** SKILL.md requires removing `worker-active` on ALL exits (done or blocked). Normal completion paths already include cleanup. The gap is in blocking (`user-input-needed`) exit paths.

**Step 1: Fix architect workflow blocking paths**

In `architect.md`, under the "If unclear" action (around line 44), update to:

```markdown
**If unclear:** Add `user-input-needed` label, remove `worker-active` label, post comment with specific questions, exit.
```

And in the Completion Signals table, update the "Unclear" row:

```markdown
| Unclear | Add `user-input-needed` to issue, remove `worker-active` (no review needed) |
```

**Step 2: Fix plan workflow blocking paths**

In `plan.md`, update both blocking exits:

In step 2 (around lines 111-117), update:
```markdown
1. Add `user-input-needed` label:
   - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
   - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
2. Post a comment explaining what needs clarification:
   - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "..." -R $OWNER/$REPO`
   - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="...")`
3. Exit immediately - do NOT add `worker-done`
```

Apply the same pattern to the step 4 blocking exit (around lines 204-210).

**Step 3: Fix review workflow blocking paths**

The review workflow doesn't have explicit `user-input-needed` paths (it always completes), so no changes needed. The existing completion path already includes `worker-active` removal.

**Step 4: Describe and advance**

```bash
jj describe -m "fix: ensure worker-active is removed in all blocking exit paths (#65)"
jj new
```

---

## Task 8: Update documentation (AGENTS.md, controller skill, state AGENTS.md)

**Files:**
- Modify: `packages/daemon/src/state/AGENTS.md`
- Modify: `.opencode/skills/legion-controller/SKILL.md`

**Depends on:** Tasks 1, 2, 3, 4, 5

**Step 1: Update state AGENTS.md ActionType table**

In `packages/daemon/src/state/AGENTS.md`, update the ActionType State Machine table to reflect changes:

- Remove the `In Progress + hasPr + no live worker → skip` row (item 11 fix)
- Add note that Retro + live worker now returns `skip` instead of `resume_implementer_for_retro`
- Add `transition_to_done` to the table
- Add note about `needs-approval` being scoped to Backlog/Todo

**Step 2: Update controller skill skip guidance**

In `.opencode/skills/legion-controller/SKILL.md`, update the skip signals table (around line 98-102):

Remove or update the row:
```markdown
| `skip` | `hasPr: true`, status: In Progress | PR opened but no `worker-done` yet; wait for implementer to finish and add `worker-done` (which triggers testing gate) |
```

Replace with:
```markdown
| `skip` | `hasPr: true`, status: In Progress, `hasLiveWorker: true` | Live implementer still working on PR; wait for it to finish |
```

Note: The `hasPr + no live worker + no worker-done` case no longer returns `skip` — it returns `dispatch_implementer` to recover from dead workers.

**Step 3: Add post-merge guidance to controller skill**

Add a note after the merge workflow section about monitoring for stranded issues:

```markdown
### Post-Merge Monitoring

If an issue remains in Retro after the merger exits, the controller should verify PR merge status:
```bash
gh pr view "$LEGION_ISSUE_ID" --json state,merged
```

If the PR is merged but the issue isn't closed, close it explicitly:
```bash
gh issue close $ISSUE_NUMBER -R $OWNER/$REPO
```

This handles edge cases where the merge workflow's explicit close failed or where GitHub auto-close didn't trigger.
```

**Step 4: Describe and advance**

```bash
jj describe -m "docs: update state machine and controller docs for hardening changes (#65)"
jj new
```

---

## Final Verification

Run full test suite and quality checks:

```bash
bun test                    # All 640+ tests pass
bunx tsc --noEmit           # No type errors
bunx biome check src/       # No lint/format issues
```

---

## Testing Plan

### Setup
- No infrastructure to boot — this is a pure state machine + documentation change
- `bun install` if dependencies aren't installed

### Health Check
- `bun test --dry-run` confirms test runner works

### Verification Steps

1. **Item 11 — deadlock recovery**
   - Action: `bun test packages/daemon/src/state/__tests__/decision-regressions.test.ts`
   - Expected: Test "dispatches implementer when In Progress issue has PR but no live worker and no worker-done" passes
   - Tool: CLI

2. **Item 12 — needs-approval scoping**
   - Action: `bun test packages/daemon/src/state/__tests__/decision.test.ts -t "needs_approval_on_non_backlog"`
   - Expected: Test passes — In Progress with leaked needs-approval follows normal flow
   - Tool: CLI

3. **Item 13 — retro spam fix**
   - Action: `bun test packages/daemon/src/state/__tests__/decision.test.ts -t "retro_with_live_worker"`
   - Expected: Returns `skip` not `resume_implementer_for_retro`
   - Tool: CLI

4. **Item 15 — sessionId accuracy**
   - Action: `bun test packages/daemon/src/state/__tests__/decision.test.ts -t "skip_with_live_worker_uses_actual"`
   - Expected: sessionId uses test mode when tester is running, not default implement
   - Tool: CLI

5. **Item 14/18 — transition_to_done exists**
   - Action: `bun test packages/daemon/src/state/__tests__/decision.test.ts -t "transition_to_done"`
   - Expected: ACTION_TO_MODE includes transition_to_done mapped to merge
   - Tool: CLI

6. **Item 16 — tools documented**
   - Action: Verify `implement.md` contains "Tools Referenced" section
   - Tool: grep/read

7. **Item 17 — worker-active cleanup**
   - Action: Verify architect.md, plan.md blocking paths include `--remove-label "worker-active"`
   - Tool: grep/read

8. **No regressions**
   - Action: `bun test && bunx tsc --noEmit && bunx biome check src/`
   - Expected: All pass with zero errors
   - Tool: CLI

### Tools Needed
- Bun test runner (`bun test`)
- TypeScript compiler (`bunx tsc`)
- Biome linter (`bunx biome check`)
- File reader for documentation verification

---

## Parallelism Annotations

- Task 1: Fix In Progress deadlock — Sequential (decision.ts)
- Task 2: Scope needs-approval — Depends on: Task 1 (same file)
- Task 3: Fix Retro spam — Depends on: Task 2 (same file)
- Task 4: Fix sessionId — Depends on: Task 3 (same file)
- Task 5: Add transition_to_done — Depends on: Task 4 (types.ts + decision.ts)
- Task 6: Document tools — Independent
- Task 7: Fix worker-active cleanup — Independent
- Task 8: Update docs — Depends on: Task 5

**Suggested waves:**
- Wave 1 (parallel): Tasks 6, 7 (documentation-only, different files)
- Wave 2 (sequential): Tasks 1 → 2 → 3 → 4 → 5 (all touch decision.ts)
- Wave 3: Task 8 (depends on wave 2)
