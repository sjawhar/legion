# State Machine Hardening Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix verified bugs in the Legion pipeline, add daemon-managed controller with duplicate guard, and align architecture docs with reality.

**Architecture:** The state machine provides deterministic suggestions + raw signals. The controller skill decides whether to follow or override them. Users customize behavior by modifying the controller skill, not the TypeScript. The daemon spawns and health-checks both the controller and workers — single authority for process lifecycle.

**Tech Stack:** TypeScript on Bun, bun:test, Biome for lint/format

**Version Control:** jj (Jujutsu), not git. Use `jj describe`, `jj new`, `jj git push`.

---

### Task 1: Fix session ID case normalization in `computeSessionId`

The daemon lowercases issue IDs before calling `computeSessionId`, but the state machine passes them as-is (uppercase from Linear). This produces different UUIDs for the same issue. The existing contract test at `packages/daemon/src/daemon/__tests__/session-id-contract.test.ts:85` is already failing.

**Files:**
- Modify: `packages/daemon/src/state/types.ts:381-392`
- Modify: `packages/daemon/src/daemon/server.ts:166`
- Test: `packages/daemon/src/daemon/__tests__/session-id-contract.test.ts`

**Step 1: Run the existing contract test to confirm it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/session-id-contract.test.ts`
Expected: FAIL — `ses_6d59...` (lowercase) vs `ses_a4c0...` (uppercase)

**Step 2: Normalize issueId inside `computeSessionId`**

In `packages/daemon/src/state/types.ts`, change line 388:

```typescript
// Before:
const name = `${issueId}:${mode}`;

// After:
const name = `${issueId.toLowerCase()}:${mode}`;
```

**Step 3: Remove redundant lowercase at daemon call site**

In `packages/daemon/src/daemon/server.ts`, remove line 166 and update line 169-172:

```typescript
// Before:
const normalizedIssueId = issueId.toLowerCase();

const port = opts.portAllocator.allocate();
const sessionId = computeSessionId(
  opts.teamId,
  normalizedIssueId,
  mode as WorkerModeLiteral
);

// After:
const port = opts.portAllocator.allocate();
const sessionId = computeSessionId(
  opts.teamId,
  issueId,
  mode as WorkerModeLiteral
);
```

Note: keep using `issueId.toLowerCase()` for the worker ID (line 177 area — `entry.id` format), just not for session ID computation.

Actually, looking at the code more carefully: `normalizedIssueId` is also used for `spawnServe` options (line 177). The worker ID format needs to stay lowercase. So instead, keep the `normalizedIssueId` variable but pass the original `issueId` to `computeSessionId`:

```typescript
const normalizedIssueId = issueId.toLowerCase();

const port = opts.portAllocator.allocate();
const sessionId = computeSessionId(
  opts.teamId,
  issueId,           // Original case — computeSessionId normalizes internally
  mode as WorkerModeLiteral
);
let entry: WorkerEntry;
try {
  entry = await opts.serveManager.spawnServe({
    issueId: normalizedIssueId,  // Keep lowercase for worker ID
    ...
```

**Step 4: Run the contract test to confirm it passes**

Run: `bun test packages/daemon/src/daemon/__tests__/session-id-contract.test.ts`
Expected: PASS

**Step 5: Run all state types tests**

Run: `bun test packages/daemon/src/state/__tests__/types.test.ts`
Expected: PASS

**Step 6: Commit**

```bash
jj describe -m "fix: normalize issueId case in computeSessionId to fix daemon/state mismatch"
jj new
```

---

### Task 2: Filter `getLiveWorkers` by worker status

`getLiveWorkers()` treats every worker entry as live regardless of status. Dead workers that haven't been cleaned up (60s health tick window) cause false `hasLiveWorker = true` → `skip`, freezing issues.

**Files:**
- Modify: `packages/daemon/src/state/fetch.ts:78-117`
- Test: `packages/daemon/src/state/__tests__/fetch.test.ts`

**Step 1: Write failing tests for status filtering**

Add to `packages/daemon/src/state/__tests__/fetch.test.ts`, inside the `getLiveWorkers` describe block:

```typescript
it("filters out dead workers", async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = mock(
    async () =>
      new Response(
        JSON.stringify([
          { id: "eng-21-implement", status: "running" },
          { id: "eng-22-plan", status: "dead" },
          { id: "eng-23-review", status: "stopped" },
        ]),
        { status: 200 }
      )
  ) as unknown as typeof fetch;

  try {
    const result = await getLiveWorkers("http://localhost:3000");
    expect(result).toEqual({ "ENG-21": "implement" });
  } finally {
    globalThis.fetch = originalFetch;
  }
});

it("includes starting workers", async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = mock(
    async () =>
      new Response(
        JSON.stringify([
          { id: "eng-21-implement", status: "starting" },
          { id: "eng-22-plan", status: "running" },
        ]),
        { status: 200 }
      )
  ) as unknown as typeof fetch;

  try {
    const result = await getLiveWorkers("http://localhost:3000");
    expect(result).toEqual({ "ENG-21": "implement", "ENG-22": "plan" });
  } finally {
    globalThis.fetch = originalFetch;
  }
});

it("handles workers without status field (backwards compat)", async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = mock(
    async () =>
      new Response(
        JSON.stringify([
          { id: "eng-21-implement" },
        ]),
        { status: 200 }
      )
  ) as unknown as typeof fetch;

  try {
    const result = await getLiveWorkers("http://localhost:3000");
    expect(result).toEqual({ "ENG-21": "implement" });
  } finally {
    globalThis.fetch = originalFetch;
  }
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: New tests FAIL (dead/stopped workers still returned)

**Step 3: Update DaemonWorker interface and filter logic**

In `packages/daemon/src/state/fetch.ts`, update the interface (line 78) and the loop (line 101):

```typescript
// Before:
interface DaemonWorker {
  id: string;
}

// After:
interface DaemonWorker {
  id: string;
  status?: string;
}
```

Add filter inside the for loop (after line 101):

```typescript
for (const worker of workers) {
  // Only include running/starting workers (or workers without status for backwards compat)
  if (worker.status && worker.status !== "running" && worker.status !== "starting") {
    continue;
  }

  // Parse worker.id format: "ISSUE-ID-mode"
  const lastDash = worker.id.lastIndexOf("-");
  if (lastDash <= 0) continue;

  const issueId = worker.id.substring(0, lastDash).toUpperCase();
  const mode = worker.id.substring(lastDash + 1);
  result[issueId] = mode;
}
```

**Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: ALL PASS

**Step 5: Commit**

```bash
jj describe -m "fix: filter getLiveWorkers by status to prevent dead-worker freeze"
jj new
```

---

### Task 3: Fix `suggestAction` — In Progress re-dispatch, Needs Review null freeze, Retro skip

Three `suggestAction` bugs where the state machine blocks the controller from acting:

(a) In Progress: no `hasPr` check → infinite re-dispatch after implement opens a PR.
(b) Needs Review: `prIsDraft === null` → `skip`, freezing the issue when GitHub API flakes.
(c) Retro: `hasLiveWorker` → `skip`, but the live worker IS the implementer to resume.

Design principle: the state machine is a thin signal provider. For (a), return `skip` (don't re-dispatch), and let the controller read the `hasPr` signal and decide what to do. For (c), remove a gate — don't block the controller.

**Files:**
- Modify: `packages/daemon/src/state/types.ts:45-61` (ActionType)
- Modify: `packages/daemon/src/state/decision.ts:21-112`
- Test: `packages/daemon/src/state/__tests__/decision.test.ts`
- Regression: `packages/daemon/src/state/__tests__/decision-regressions.test.ts` (already failing — must pass after)

**Step 1: Run the regression tests to confirm they fail**

Run: `bun test packages/daemon/src/state/__tests__/decision-regressions.test.ts`
Expected: 2 FAIL — In Progress returns `dispatch_implementer` (wants `skip`), Retro returns `skip` (wants `resume_implementer_for_retro`)

**Step 2: Add `retry_pr_check` to ActionType**

In `packages/daemon/src/state/types.ts`, add to the ActionType union (after line 60):

```typescript
export type ActionType =
  | "skip"
  | "investigate_no_pr"
  | "dispatch_architect"
  | "dispatch_planner"
  | "dispatch_implementer"
  | "dispatch_reviewer"
  | "dispatch_merger"
  | "resume_implementer_for_changes"
  | "resume_implementer_for_retro"
  | "transition_to_in_progress"
  | "transition_to_needs_review"
  | "transition_to_retro"
  | "transition_to_todo"
  | "relay_user_feedback"
  | "remove_worker_active_and_redispatch"
  | "add_needs_approval"
  | "retry_pr_check";
```

**Step 3: Update `suggestAction` in decision.ts — three cases**

In `packages/daemon/src/state/decision.ts`:

**(a) IN_PROGRESS** — add `hasPr` guard that returns `skip` (not `transition_to_needs_review`; let controller/Linear handle the status change):

```typescript
case IssueStatus.IN_PROGRESS:
  if (hasWorkerDone) {
    return "transition_to_needs_review";
  }
  if (hasPr && !hasLiveWorker) {
    return "skip";
  }
  if (hasLiveWorker) {
    return "skip";
  }
  return "dispatch_implementer";
```

**(b) NEEDS_REVIEW** — replace `skip` with `retry_pr_check` when `prIsDraft === null`:

```typescript
case IssueStatus.NEEDS_REVIEW:
  if (hasWorkerDone) {
    if (!hasPr) {
      return "investigate_no_pr";
    }
    if (prIsDraft === null) {
      return "retry_pr_check";
    }
    if (prIsDraft) {
      return "resume_implementer_for_changes";
    }
    return "transition_to_retro";
  }
  if (hasLiveWorker) {
    return "skip";
  }
  return "dispatch_reviewer";
```

**(c) RETRO** — remove the `hasLiveWorker` → `skip` guard. In Retro, a live worker is the implementer waiting to be resumed, not a reason to skip:

```typescript
case IssueStatus.RETRO:
  if (hasWorkerDone) {
    return "dispatch_merger";
  }
  return "resume_implementer_for_retro";
```

**Step 4: Add `retry_pr_check` to ACTION_TO_MODE**

In `packages/daemon/src/state/decision.ts`, add to the ACTION_TO_MODE record:

```typescript
retry_pr_check: WorkerMode.REVIEW,
```

**Step 5: Update existing tests in decision.test.ts**

The test `needs_review_worker_done_has_pr_but_unknown_status skips` (line 54-57) now expects `retry_pr_check`:

```typescript
it("needs_review_worker_done_has_pr_but_unknown_status retries pr check", () => {
  const action = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, null, true);
  expect(action).toBe("retry_pr_check");
});
```

The test `retro_with_live_worker skips` (line 116-119) now expects `resume_implementer_for_retro`:

```typescript
it("retro_with_live_worker resumes implementer", () => {
  const action = suggestAction(IssueStatus.RETRO, false, true, null, false);
  expect(action).toBe("resume_implementer_for_retro");
});
```

**Step 6: Add new tests**

Add to the `suggestAction` describe block in `decision.test.ts`:

```typescript
// Ensure retry_pr_check is distinct from skip — it must not collapse back to skip
it("retry_pr_check_is_distinct_from_skip", () => {
  const retry = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, null, true);
  const skip = suggestAction(IssueStatus.DONE, false, false, null, false);
  expect(retry).toBe("retry_pr_check");
  expect(skip).toBe("skip");
  expect(retry).not.toBe(skip);
});

it("in_progress_has_pr_no_worker_done_no_live_worker_skips", () => {
  const action = suggestAction(IssueStatus.IN_PROGRESS, false, false, null, true);
  expect(action).toBe("skip");
});
```

**Step 7: Run all decision tests including regressions**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts packages/daemon/src/state/__tests__/decision-regressions.test.ts`
Expected: ALL PASS

**Step 8: Commit**

```bash
jj describe -m "fix: In Progress hasPr skip, Needs Review retry_pr_check, Retro remove hasLiveWorker gate"
jj new
```

---

### Task 4: Remove `preCheck` from state machine

Quality gate is controller policy, not state-machine-level data. The controller skill already knows when to run quality checks.

**Files:**
- Modify: `packages/daemon/src/state/types.ts:39-40, 290-300, 312-322, 324-343`
- Modify: `packages/daemon/src/state/decision.ts:8-18, 143-154`
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts:358-448`

**Step 1: Remove PreCheckType from types.ts**

Remove the type definition (line 39-40):
```typescript
// Delete:
export type PreCheckType = "quality-gate";
```

Remove `preCheck` from `IssueStateDict` (line 297):
```typescript
// Delete this line from IssueStateDict interface:
preCheck?: PreCheckType;
```

Remove `preCheck` from `IssueState` (line 319):
```typescript
// Delete this line from IssueState interface:
preCheck?: PreCheckType;
```

Remove `preCheck` handling from `IssueState.toDict` (lines 339-341):
```typescript
// Delete these lines:
if (state.preCheck) {
  dict.preCheck = state.preCheck;
}
```

**Step 2: Remove preCheck from decision.ts**

Remove the import of `PreCheckType` (line 16):
```typescript
// Remove PreCheckType from the import
```

Remove preCheck computation (lines 143-144):
```typescript
// Delete:
const preCheck: PreCheckType | undefined =
  action === "transition_to_needs_review" ? "quality-gate" : undefined;
```

Remove preCheck from the return object (line 153):
```typescript
// Delete:
preCheck,
```

**Step 3: Remove or update preCheck tests**

Delete the entire `describe("quality gate pre-check", ...)` block (lines 358-448) from `decision.test.ts`. These 4 tests test removed functionality.

**Step 4: Run tests**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: ALL PASS (fewer tests, but no failures)

**Step 5: Run type checker**

Run: `bunx tsc --noEmit`
Expected: No errors

**Step 6: Commit**

```bash
jj describe -m "refactor: remove preCheck from state machine (quality gate is controller policy)"
jj new
```

---

### Task 5: Expand state output with raw signals

The state CLI currently returns only `suggestedAction`. Adding raw signals lets the controller make informed decisions even when the suggestion is wrong.

**Files:**
- Modify: `packages/daemon/src/state/types.ts` (IssueState, IssueStateDict, FetchedIssueData)
- Modify: `packages/daemon/src/state/fetch.ts` (return worker mode/status)
- Modify: `packages/daemon/src/state/decision.ts` (pass through signals)
- Test: `packages/daemon/src/state/__tests__/decision.test.ts`

**Step 1: Add `workerMode` and `workerStatus` to FetchedIssueData**

In `packages/daemon/src/state/types.ts`, add to `FetchedIssueData` (after line 280):

```typescript
export interface FetchedIssueData {
  issueId: string;
  status: IssueStatusLiteral | string;
  labels: string[];
  hasPr: boolean;
  prIsDraft: boolean | null;
  hasLiveWorker: boolean;
  workerMode: string | null;    // NEW
  workerStatus: string | null;  // NEW
  hasUserFeedback: boolean;
  hasUserInputNeeded: boolean;
  hasNeedsApproval: boolean;
  hasHumanApproved: boolean;
}
```

**Step 2: Add signals to IssueState and IssueStateDict**

In `IssueStateDict`, add after `hasLiveWorker`:

```typescript
workerMode: string | null;
workerStatus: string | null;
```

In `IssueState`, add the same two fields after `hasLiveWorker`.

Update `IssueState.toDict` to include them:

```typescript
workerMode: state.workerMode,
workerStatus: state.workerStatus,
```

**Step 3: Update `getLiveWorkers` to return richer data**

Change the return type from `Record<string, string>` to `Record<string, { mode: string; status: string }>`:

In `packages/daemon/src/state/fetch.ts`:

```typescript
export async function getLiveWorkers(
  daemonUrl: string
): Promise<Record<string, { mode: string; status: string }>> {
  try {
    const response = await fetch(`${daemonUrl}/workers`);
    if (!response.ok) {
      return {};
    }

    const workers = (await response.json()) as DaemonWorker[];
    const result: Record<string, { mode: string; status: string }> = {};

    for (const worker of workers) {
      if (worker.status && worker.status !== "running" && worker.status !== "starting") {
        continue;
      }

      const lastDash = worker.id.lastIndexOf("-");
      if (lastDash <= 0) continue;

      const issueId = worker.id.substring(0, lastDash).toUpperCase();
      const mode = worker.id.substring(lastDash + 1);
      result[issueId] = { mode, status: worker.status ?? "running" };
    }

    return result;
  } catch {
    return {};
  }
}
```

**Step 4: Update `fetchAllIssueData` to use richer worker data**

In `packages/daemon/src/state/fetch.ts`, update the results loop (around line 407):

```typescript
for (const issue of parsedIssues) {
  const workerInfo = liveWorkers[issue.issueId.toUpperCase()] ?? null;
  const hasLiveWorker = workerInfo !== null;
  const prIsDraft: boolean | null = prDraftMap[issue.issueId] ?? null;

  results.push({
    issueId: issue.issueId,
    status: issue.status,
    labels: issue.labels,
    hasPr: issue.hasPr,
    prIsDraft,
    hasLiveWorker,
    workerMode: workerInfo?.mode ?? null,
    workerStatus: workerInfo?.status ?? null,
    hasUserFeedback: issue.hasUserFeedback,
    hasUserInputNeeded: issue.hasUserInputNeeded,
    hasNeedsApproval: issue.hasNeedsApproval,
    hasHumanApproved: issue.hasHumanApproved,
  });
}
```

**Step 5: Pass through signals in `buildIssueState`**

In `packages/daemon/src/state/decision.ts`, add to the return object in `buildIssueState`:

```typescript
return {
  status: data.status,
  labels: data.labels,
  hasPr: data.hasPr,
  prIsDraft: data.prIsDraft,
  hasLiveWorker: data.hasLiveWorker,
  workerMode: data.workerMode,
  workerStatus: data.workerStatus,
  suggestedAction: action,
  sessionId,
  hasUserFeedback: data.hasUserFeedback,
};
```

**Step 6: Update all FetchedIssueData test fixtures**

Every test that creates `FetchedIssueData` objects needs the new fields added with defaults:

```typescript
workerMode: null,
workerStatus: null,
```

This applies to all fixtures in `decision.test.ts` and `fetch.test.ts`. There are ~25 fixtures to update — add `workerMode: null, workerStatus: null` to each.

**Step 7: Update `getLiveWorkers` tests**

The existing tests return `Record<string, string>`. Update them to use the new format:

```typescript
// Before:
expect(result).toEqual({ "ENG-21": "implement", "ENG-22": "plan" });

// After:
expect(result).toEqual({
  "ENG-21": { mode: "implement", status: "running" },
  "ENG-22": { mode: "plan", status: "running" },
});
```

Update all `getLiveWorkers` test expectations to use the `{ mode, status }` shape.

**Step 8: Run all tests**

Run: `bun test packages/daemon/src/state/__tests__/`
Expected: ALL PASS

**Step 9: Run type checker**

Run: `bunx tsc --noEmit`
Expected: No errors

**Step 10: Commit**

```bash
jj describe -m "feat: expose raw signals (workerMode, workerStatus) in state output"
jj new
```

---

### Task 6: Fix task lock — PID-based liveness + release before sync

Two fixes to the task lock:
(a) The staleness check uses timestamp-only, so a legitimately held lock can be stolen after 30s. Fix: add PID to the lock file and check process liveness before reclaiming.
(b) The lock is held during `syncTaskTodoUpdate` (async network call). Fix: release lock before the sync call, fire-and-forget.

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/storage.ts:94-172`
- Modify: `packages/opencode-plugin/src/tools/task/task-claim.ts:26-103`
- Modify: `packages/opencode-plugin/src/tools/task/task-update.ts:57-157`
- Regression: `packages/opencode-plugin/src/tools/task/__tests__/storage-regressions.test.ts` (already failing — must pass after)

**Step 1: Run the regression test to confirm it fails**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/storage-regressions.test.ts`
Expected: FAIL — `lock2.acquired` is `true` (should be `false`)

**Step 2: Add a helper to read process start time**

PID alone isn't enough — if the holder dies and the PID gets reused by a new process, `kill(pid, 0)` would still return true. Store `pid + starttime` to uniquely identify the holder process. On Linux, read `/proc/<pid>/stat` field 22 (starttime in clock ticks).

Add a helper at the top of `storage.ts` (after the imports):

```typescript
/**
 * Read process start time from /proc to guard against PID reuse.
 * Returns null on non-Linux or if the proc file is unreadable.
 */
function getProcessStartTime(pid: number): string | null {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
    // Field 22 (1-indexed) is starttime; fields are space-separated
    // but field 2 (comm) can contain spaces inside parens, so find closing paren first
    const afterComm = stat.indexOf(") ");
    if (afterComm === -1) return null;
    const fields = stat.substring(afterComm + 2).split(" ");
    // starttime is field 22 overall, which is index 19 after the 3 fields we skipped (pid, comm, state)
    return fields[19] ?? null;
  } catch {
    return null;
  }
}
```

**Step 3: Write PID + starttime into lock file**

In `packages/opencode-plugin/src/tools/task/storage.ts`, update `createLockExclusive` (line 98-103):

```typescript
const startTime = getProcessStartTime(process.pid);
const createLockExclusive = (timestamp: number) => {
  writeFileSync(
    lockPath,
    JSON.stringify({ id: lockId, timestamp, pid: process.pid, startTime }),
    { encoding: "utf-8", flag: "wx" }
  );
};
```

**Step 4: Add PID + starttime liveness check to `isStale`**

In `packages/opencode-plugin/src/tools/task/storage.ts`, update `isStale` (line 105-114):

```typescript
const isStale = () => {
  try {
    const lockContent = readFileSync(lockPath, "utf-8");
    const lockData = JSON.parse(lockContent);
    const lockAge = Date.now() - lockData.timestamp;
    if (lockAge <= STALE_LOCK_THRESHOLD_MS) return false;

    // Timestamp is old — check if holding process is still alive
    if (typeof lockData.pid === "number") {
      try {
        process.kill(lockData.pid, 0); // signal 0 = existence check
      } catch (err: unknown) {
        // ESRCH = no such process → dead → stale
        // EPERM = process exists but we can't signal → alive → not stale
        if (err && typeof err === "object" && "code" in err) {
          return (err as { code: string }).code === "ESRCH";
        }
        return true; // Unknown error, assume stale
      }

      // Process with this PID exists. Verify it's the same process (not PID reuse).
      if (lockData.startTime != null) {
        const currentStartTime = getProcessStartTime(lockData.pid);
        if (currentStartTime !== null && currentStartTime !== lockData.startTime) {
          return true; // PID reused by a different process → stale
        }
      }

      return false; // Same process still alive → not stale
    }

    return true; // No PID (old format) — fall back to timestamp-only
  } catch {
    return true;
  }
};
```

Key behaviors:
- `kill(pid, 0)` succeeds → process exists; verify `startTime` matches to rule out PID reuse
- `kill(pid, 0)` throws `ESRCH` → process dead → lock is stale
- `kill(pid, 0)` throws `EPERM` → process exists but different user → treat as alive (don't steal)
- No `startTime` in lock (backwards compat / non-Linux) → fall back to PID-only check

**Step 5: Run the regression test**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/storage-regressions.test.ts`
Expected: PASS — lock2 sees the stale timestamp, checks the PID (still alive, same startTime), returns `acquired: false`

**Step 6: Add additional regression tests**

Add to `packages/opencode-plugin/src/tools/task/__tests__/storage-regressions.test.ts`:

```typescript
it("reclaims a stale lock when the holding process is dead", () => {
  const lock1 = acquireLock(tempDir);
  expect(lock1.acquired).toBe(true);
  const lockPath = path.join(tempDir, ".lock");

  // Simulate: holder died, PID no longer exists
  const lockContent = JSON.parse(fs.readFileSync(lockPath, "utf-8"));
  fs.writeFileSync(
    lockPath,
    JSON.stringify({
      ...lockContent,
      timestamp: Date.now() - 60_000,
      pid: 2_147_483_647, // Very unlikely to be a real running process
      startTime: "99999999999",
    }),
    "utf-8"
  );

  const lock2 = acquireLock(tempDir);
  expect(lock2.acquired).toBe(true);

  lock1.release(); // no-op (lock was reclaimed)
  lock2.release();
});
```

**Step 7: Run full storage tests**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/storage.test.ts packages/opencode-plugin/src/tools/task/__tests__/storage-regressions.test.ts`
Expected: ALL PASS

**Step 6: Fix task-claim.ts — release lock before sync**

Restructure the try/finally in `task-claim.ts`:

```typescript
// Before (line 26-103):
try {
  // ... all logic ...
  await syncTaskTodoUpdate(ctx, validated, context.sessionID);
  return JSON.stringify({ task: validated });
} finally {
  lock.release();
}

// After:
let result: string;
try {
  // ... all logic up to and including writeJsonAtomic ...
  result = JSON.stringify({ task: validated });
} finally {
  lock.release();
}
// Fire-and-forget sync outside lock
syncTaskTodoUpdate(ctx, validated, context.sessionID).catch(() => {});
return result;
```

**Step 7: Fix task-create.ts — release lock before sync**

Same bug exists in `task-create.ts` (line 121-133): `syncTaskTodoUpdate` is awaited inside the lock.

```typescript
// Before (packages/opencode-plugin/src/tools/task/task-create.ts:110-133):
writeJsonAtomic(join(taskDir, `${taskId}.json`), validatedTask);
// ... bidirectional blocks sync ...
await syncTaskTodoUpdate(ctx, validatedTask, context.sessionID);
// ... build result ...
return JSON.stringify(result);
} finally {
  lock.release();
}

// After:
writeJsonAtomic(join(taskDir, `${taskId}.json`), validatedTask);
// ... bidirectional blocks sync ...
const result: Record<string, unknown> = {
  task: { id: validatedTask.id, subject: validatedTask.subject },
};
if (warnings.length > 0) {
  result.warnings = warnings;
}
} finally {
  lock.release();
}
// Fire-and-forget sync outside lock
syncTaskTodoUpdate(ctx, validatedTask, context.sessionID).catch(() => {});
return JSON.stringify(result);
```

**Step 8: Fix task-update.ts — release lock before sync**

Same pattern. Move `lock.release()` before `syncTaskTodoUpdate`:

```typescript
// After:
let resultPayload: Record<string, unknown>;
try {
  // ... all logic up to and including writeJsonAtomic (line 136) + addBlocks sync (138-147) ...
  resultPayload = { task: validatedTask };
  if (warnings.length > 0) {
    resultPayload.warnings = warnings;
  }
} finally {
  lock.release();
}
// Fire-and-forget sync outside lock
syncTaskTodoUpdate(ctx, validatedTask, context.sessionID).catch(() => {});
return JSON.stringify(resultPayload);
```

**Step 9: Run all task tests**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/`
Expected: ALL PASS

**Step 9: Commit**

```bash
jj describe -m "fix: PID-based lock liveness check + release lock before syncTaskTodoUpdate"
jj new
```

---

### Task 7: Fix asymmetric dependency graph in `addBlockedBy`

`addBlockedBy` updates the current task's `blockedBy` but doesn't update the upstream task's `blocks`, breaking the invariant `A.blocks contains B iff B.blockedBy contains A`.

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/task-update.ts:80-101`

**Step 1: Add upstream blocks sync to addBlockedBy handler**

After line 100 (`task.blockedBy = newBlockedBy;`), add the symmetric update:

```typescript
task.blockedBy = newBlockedBy;

// Sync upstream tasks' blocks arrays (mirror of addBlocks logic)
for (const depId of addBlockedBy) {
  const depPath = join(taskDir, `${depId}.json`);
  const depTask = readJsonSafe(depPath, TaskSchema);
  if (depTask) {
    depTask.blocks = [...new Set([...depTask.blocks, validated.id])];
    writeJsonAtomic(depPath, TaskSchema.parse(depTask));
  }
}
```

**Step 2: Run tests**

Run: `bun test`
Expected: ALL PASS

**Step 3: Commit**

```bash
jj describe -m "fix: addBlockedBy now updates upstream task's blocks array (symmetric deps)"
jj new
```

---

### Task 8: Clear lease metadata on task completion

Completed tasks retain stale `lease_expires_at` and `claimed_by_session` fields.

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/task-update.ts:65-69`

**Step 1: Add lease cleanup on terminal status**

After the status update (line 67), add cleanup:

```typescript
if (validated.status !== undefined) task.status = validated.status;

// Clear lease metadata on terminal statuses
if (validated.status === "completed" || validated.status === "cancelled") {
  if (task.metadata) {
    delete task.metadata.lease_expires_at;
    delete task.metadata.claimed_by_session;
  }
}
```

**Step 2: Run tests**

Run: `bun test`
Expected: ALL PASS

**Step 3: Commit**

```bash
jj describe -m "fix: clear lease metadata when task completes or is cancelled"
jj new
```

---

### Task 9: Update controller skill markdown

Remove preCheck references, document signals, clarify quality gate as controller policy.

**Files:**
- Modify: `.claude/skills/legion-controller/SKILL.md:62-117`

**Step 1: Update the state machine output documentation**

Replace the quality gate section (lines 90-117) with controller-owned policy:

Replace:
```
### Quality Gate Check

When the state machine returns `preCheck: "quality-gate"` ...
```

With:
```
### Quality Gate (Controller Policy)

Before transitioning from In Progress → Needs Review, the controller independently verifies code quality. This is a controller-level policy, not signaled by the state machine.

**When to run:** Whenever executing a `transition_to_needs_review` action.
```

Keep the bash commands for running tests/tsc/biome, and the pass/fail behavior. Just remove all references to `preCheck`.

**Step 2: Document the signals object and controller reasoning**

After the state script invocation (line 63), add a note about the enriched output:

```
The state CLI returns JSON with both `suggestedAction` and raw signals:
- `hasLiveWorker`, `workerMode`, `workerStatus` — worker state
- `hasPr`, `prIsDraft` — PR state
- `hasUserFeedback` — user interaction state

Use `suggestedAction` as the primary guide, but consult raw signals when the suggestion
is `skip`. The state machine returns `skip` conservatively — the controller should reason
about what to do:

| suggestedAction | Signals | Controller should... |
|-----------------|---------|---------------------|
| `skip` | `hasPr: true`, status: In Progress | PR opened; wait for Linear auto-transition or manually advance to Needs Review |
| `skip` | `workerStatus: "dead"` | Dead worker blocking progress; clean up and re-evaluate |
| `retry_pr_check` | `prIsDraft: null` | GitHub API flaked; try again next iteration |
```

**Step 3: Document `retry_pr_check` action**

In the state transitions table, add:
```
| Needs Review + worker-done (PR status unknown) | `retry_pr_check` - do NOT dispatch any worker; wait and re-check next iteration |
```

Add explicit guidance in the controller skill that `retry_pr_check` is a no-op action:

```
**`retry_pr_check`:** The GitHub API couldn't determine PR draft status. Do nothing this iteration —
don't dispatch a worker, don't transition status. The next loop iteration will re-run the state script
which will retry the GitHub API call. If this persists across multiple iterations, investigate the
GitHub API connectivity.
```

**Step 4: Commit**

```bash
jj describe -m "docs: update controller skill — remove preCheck, document signals, quality gate as policy"
jj new
```

---

### Task 10: Clean up implement workflow docs

Remove contradictory worker-done references, clarify sub-mode vs daemon mode.

**Files:**
- Modify: `.claude/skills/legion-worker/workflows/implement.md:5-11, 89-93, 135-137`

**Step 1: Add clarifying note about daemon modes vs prompt sub-modes**

At the top of the Mode Detection section (after line 11), add:

```
> **Note:** The daemon API mode is always `implement`. The sub-mode (fresh vs address-comments) is conveyed in the controller's prompt text, not the API call.
```

**Step 2: Remove contradictory worker-done reference**

Replace lines 89-92:
```
Record the results as evidence for the controller's quality gate verification. Include in your Linear comment when adding `worker-done`:
```
With:
```
Record the results as evidence for the controller's quality gate verification. Include in your Linear comment:
```

**Step 3: Commit**

```bash
jj describe -m "docs: clarify implement workflow — daemon mode vs sub-mode, remove worker-done refs"
jj new
```

---

### Task 11: Update retro workflow to use `background_task`

Replace `opencode run` with the `background_task` tool pattern.

**Files:**
- Modify: `.claude/skills/legion-worker/workflows/retro.md:20-34`

**Step 1: Replace opencode run with background_task**

Replace lines 20-34:

```markdown
### 2. Launch Background Subagent (Parallel)

Use `background_task` tool to spawn a fresh subagent:

- **Category:** `unspecified-low`
- **Description:** "Retro analysis for $LINEAR_ISSUE_ID"
- **Prompt:**

> You are analyzing a completed PR to capture learnings.
>
> Issue: $LINEAR_ISSUE_ID
> PR: $PR_URL
>
> 1. Fetch the PR diff and description via gh pr view and gh pr diff
> 2. Invoke /compound-engineering:workflows:compound to document learnings
> 3. Write output to docs/solutions/ in the current directory
>
> Focus on patterns that would help future implementations.
```

**Step 2: Commit**

```bash
jj describe -m "docs: update retro workflow to use background_task instead of opencode run"
jj new
```

---

### Task 12: Cross-family review — make model configurable

The delegation tool already accepts `model` override. The framework is ready. The fix is ensuring the implement workflow explicitly uses it.

**Files:**
- Modify: `.claude/skills/legion-worker/workflows/implement.md:94-117`

**Step 1: Update cross-family review section**

The current text (line 100) says "Use a different model family than the one that implemented the code" but doesn't specify how. Make it explicit:

Replace lines 98-101:
```
1. Spawn a review session:
   - Category: `review-implementation`
   - Model override: Use a different model family than the one that implemented the code
   - Prompt: Include:
```

With:
```
1. Spawn a review session using `background_task`:
   - Category: `review-implementation`
   - Model: Specify an explicit model from a different provider (e.g., `google/gemini-3-pro` or `openai/gpt-5.2-codex`)
   - Prompt: Include:
```

**Step 2: Commit**

```bash
jj describe -m "docs: make cross-family review model explicit in implement workflow"
jj new
```

---

### Task 13: Daemon spawns and manages the controller

The controller is currently spawned externally. It should be managed by the daemon like any other worker — same spawn, health-check, and deduplication. Since `computeControllerSessionId(teamId)` is deterministic, the daemon can prevent duplicate controllers by ID.

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts` (spawn controller after server starts)
- Modify: `packages/daemon/src/daemon/server.ts` (add duplicate worker guard to POST /workers)
- Modify: `packages/daemon/src/state/types.ts` (import `computeControllerSessionId`)
- Test: `packages/daemon/src/daemon/__tests__/server.test.ts`

**Step 1: Add duplicate worker guard to POST /workers**

In `packages/daemon/src/daemon/server.ts`, before spawning (around line 166), check if a worker with the same ID already exists:

```typescript
const workerId = `${normalizedIssueId}-${mode}`.toLowerCase();
const existing = workers.get(workerId);
if (existing) {
  return jsonResponse(
    { error: "worker_already_exists", id: workerId, port: existing.port, sessionId: existing.sessionId },
    409
  );
}
```

This prevents duplicate workers AND duplicate controllers — same mechanism.

**Step 2: Write a test for the duplicate guard**

```typescript
it("rejects duplicate worker for same issue+mode", async () => {
  // First spawn succeeds
  const res1 = await fetch(`${baseUrl}/workers`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ issueId: "ENG-1", mode: "implement", workspace: "/tmp" }),
  });
  expect(res1.status).toBe(200);

  // Second spawn for same issue+mode returns 409
  const res2 = await fetch(`${baseUrl}/workers`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ issueId: "ENG-1", mode: "implement", workspace: "/tmp" }),
  });
  expect(res2.status).toBe(409);
  const body = await res2.json();
  expect(body.error).toBe("worker_already_exists");
});
```

**Step 3: Run tests**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: ALL PASS

**Step 4: Spawn controller in `startDaemon`**

In `packages/daemon/src/daemon/index.ts`, after the server starts (after line 186), spawn the controller:

```typescript
const { server, stop } = resolvedDeps.startServer({ ... });
stopServer = stop;

const baseUrl = `http://127.0.0.1:${server.port}`;

// Spawn controller as a managed worker
const controllerPort = resolvedDeps.portAllocator.allocate();
const controllerSessionId = computeControllerSessionId(config.teamId);
try {
  const controllerEntry = await resolvedDeps.serveManager.spawnServe({
    issueId: "controller",
    mode: "controller",
    workspace: config.legionDir,
    port: controllerPort,
    sessionId: controllerSessionId,
    env: {
      LINEAR_TEAM_ID: config.teamId,
      LEGION_DIR: config.legionDir,
      LEGION_DAEMON_PORT: String(server.port),
      LEGION_SHORT_ID: config.shortId ?? "default",
    },
  });
  // Register in workers map via HTTP (so it's tracked like any other worker)
  await resolvedDeps.fetch(`${baseUrl}/workers`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      issueId: "controller",
      mode: "controller",
      workspace: config.legionDir,
    }),
  });
} catch (error) {
  console.error(`Failed to spawn controller: ${error}`);
  resolvedDeps.portAllocator.release(controllerPort);
}
```

Wait — that would double-spawn because POST /workers also calls spawnServe. The cleaner approach: just POST to /workers like the controller is any other worker. The endpoint handles spawning + tracking:

```typescript
// After server starts, spawn controller via the HTTP API (single source of truth)
try {
  const controllerRes = await resolvedDeps.fetch(`${baseUrl}/workers`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      issueId: "controller",
      mode: "controller",
      workspace: config.legionDir,
      env: {
        LINEAR_TEAM_ID: config.teamId,
        LEGION_DIR: config.legionDir,
        LEGION_DAEMON_PORT: String(server.port),
        LEGION_SHORT_ID: config.shortId ?? "default",
      },
    }),
  });
  if (controllerRes.ok) {
    const data = await controllerRes.json();
    console.log(`Controller spawned: session=${data.sessionId} port=${data.port}`);
  }
} catch (error) {
  console.error(`Failed to spawn controller: ${error}`);
}
```

This uses the daemon's own HTTP API to spawn the controller — same path as any worker. The duplicate guard prevents a second controller. The health tick monitors it. If it dies, the health tick marks it dead and cleans it up.

**Step 5: Handle controller mode in server.ts session ID computation**

The POST /workers handler uses `computeSessionId(teamId, issueId, mode)` for workers. For the controller, it should use `computeControllerSessionId(teamId)` instead:

```typescript
// In POST /workers handler, after validation:
const sessionId = mode === "controller"
  ? computeControllerSessionId(opts.teamId)
  : computeSessionId(opts.teamId, issueId, mode as WorkerModeLiteral);
```

Import `computeControllerSessionId` from `../state/types`.

**Step 6: Send initial prompt to controller**

After the controller is spawned, send the initial prompt to load the controller skill:

```typescript
if (controllerRes.ok) {
  const data = await controllerRes.json();
  const controllerPort = data.port;
  const controllerSessionId = data.sessionId;

  // Send initial prompt to start the controller loop
  await resolvedDeps.fetch(
    `http://127.0.0.1:${controllerPort}/session/${controllerSessionId}/prompt_async`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        parts: [{ type: "text", text: "/legion-controller" }],
      }),
    }
  );
  console.log(`Controller started: session=${controllerSessionId} port=${controllerPort}`);
}
```

**Step 7: Run all daemon tests**

Run: `bun test packages/daemon/src/daemon/__tests__/`
Expected: ALL PASS

**Step 8: Commit**

```bash
jj describe -m "feat: daemon spawns and manages controller, add duplicate worker guard"
jj new
```

---

### Task 14: Capture worker stderr for spawn-failure diagnostics

Worker processes use `stdio: ["ignore", "ignore", "ignore"]`. Once OpenCode initializes, the session is fully observable via attach/API. But if the process crashes *during startup* (bad port, missing deps, corrupt workspace), there's zero diagnostic info — just a binary dead/alive from health check.

Fix: capture stderr to a per-worker log file. Stdout can stay ignored (OpenCode handles its own output).

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts:21-29`
- Modify: `packages/daemon/src/daemon/config.ts` (add logDir to DaemonConfig)

**Step 1: Add logDir to DaemonConfig**

In `packages/daemon/src/daemon/config.ts`, add:

```typescript
logDir: string;  // Directory for worker stderr logs
```

Default: `path.join(stateDir, "logs")`. Ensure directory exists on startup.

**Step 2: Capture stderr in spawnServe**

In `packages/daemon/src/daemon/serve-manager.ts`, replace `stdio: ["ignore", "ignore", "ignore"]`:

```typescript
import { openSync, mkdirSync } from "node:fs";
import { join } from "node:path";

export async function spawnServe(opts: SpawnOptions & { logDir?: string }): Promise<WorkerEntry> {
  let stderr: "ignore" | number = "ignore";
  if (opts.logDir) {
    mkdirSync(opts.logDir, { recursive: true });
    const logFile = join(opts.logDir, `${opts.issueId}-${opts.mode}.stderr.log`);
    stderr = openSync(logFile, "a");  // Append mode
  }

  const subprocess = Bun.spawn(["opencode", "serve", "--port", String(opts.port)], {
    cwd: opts.workspace,
    env: { ...process.env, ...opts.env },
    stdio: ["ignore", "ignore", stderr],
  });
  // ...
```

**Step 3: Run tests**

Run: `bun test packages/daemon/src/daemon/__tests__/`
Expected: ALL PASS

**Step 4: Commit**

```bash
jj describe -m "feat: capture worker stderr to log files for spawn-failure diagnostics"
jj new
```

---

### Task 15: Add crash counting and escalation

When a worker crashes repeatedly (corrupt workspace, OOM, bad jj state), the `remove_worker_active_and_redispatch` action fires indefinitely. No crash counter, no backoff, no escalation to a human.

Fix: add `crashCount` and `lastCrashAt` to `WorkerEntry`. The health tick increments `crashCount` when marking a worker dead. After 3 crashes for the same issue+mode, the daemon adds `user-input-needed` label to the Linear issue instead of allowing another dispatch.

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts` (WorkerEntry type)
- Modify: `packages/daemon/src/daemon/index.ts` (health tick crash counting)
- Modify: `packages/daemon/src/daemon/server.ts` (persist crashCount across re-spawns)
- Modify: `packages/daemon/src/daemon/state-file.ts` (crash history persisted to disk)

**Step 1: Add crash tracking fields to WorkerEntry**

In `packages/daemon/src/daemon/serve-manager.ts`:

```typescript
export interface WorkerEntry {
  id: string;
  port: number;
  pid: number;
  sessionId: string;
  startedAt: string;
  status: "starting" | "running" | "stopped" | "dead";
  crashCount: number;      // NEW
  lastCrashAt: string | null;  // NEW
}
```

Default `crashCount: 0` and `lastCrashAt: null` in `spawnServe`.

**Step 2: Increment crash count in health tick**

In `packages/daemon/src/daemon/index.ts`, when the health tick marks a worker as dead:

```typescript
if (!healthy) {
  // Before deleting, increment crash count for this worker ID
  await fetchFn(`${baseUrl}/workers/${entry.id}`, {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      status: "dead",
      crashCount: (entry.crashCount ?? 0) + 1,
      lastCrashAt: new Date().toISOString(),
    }),
  });
  await fetchFn(`${baseUrl}/workers/${entry.id}`, { method: "DELETE" });
}
```

**Step 3: Check crash count before spawning**

In `packages/daemon/src/daemon/server.ts`, in the `POST /workers` handler, check crash history before spawning:

```typescript
const MAX_CRASHES = 3;

// Check crash history (persisted in state file)
const crashHistory = crashCounts.get(workerId) ?? 0;
if (crashHistory >= MAX_CRASHES) {
  return jsonResponse(
    { error: "crash_limit_exceeded", id: workerId, crashCount: crashHistory,
      message: "Worker has crashed too many times. Add user-input-needed label." },
    429
  );
}
```

The controller receives this 429 and can add `user-input-needed` to the Linear issue, escalating to a human.

**Step 4: Persist crash counts across daemon restarts**

Store crash counts in the state file alongside worker entries. When a worker is deleted, its crash count is preserved in a separate `crashHistory` map so it survives re-spawn attempts.

**Step 5: Run tests**

Run: `bun test packages/daemon/src/daemon/__tests__/`
Expected: ALL PASS

**Step 6: Commit**

```bash
jj describe -m "feat: crash counting and escalation — max 3 crashes before human intervention"
jj new
```

---

### Task 16: Remove transition table duplication from controller skill

The controller SKILL.md contains a state transition table that duplicates `decision.ts`. When someone adds a new action to the code, they must also update the skill. This will drift.

Fix: replace the transition table with intent-based routing. The controller doesn't need to know every action — it just needs to know the patterns: `dispatch_*` → spawn worker, `transition_to_*` → update Linear status, `resume_*` → send prompt to existing worker, `skip` → check raw signals for edge cases.

This aligns with the architecture framing: TS owns the decision table, the skill routes generically.

**Files:**
- Modify: `.claude/skills/legion-controller/SKILL.md`

**Step 1: Replace transition table with intent-based routing**

Remove the state transitions table (lines ~66-80) and replace with:

```markdown
### Routing by Action Intent

The state machine returns a `suggestedAction`. Route by prefix:

| Prefix | Intent | Controller action |
|--------|--------|-------------------|
| `dispatch_` | Spawn a new worker | `POST /workers` with mode from `ACTION_TO_MODE` |
| `transition_to_` | Move issue to new status | Update Linear issue status |
| `resume_` | Send prompt to existing worker | Find worker by sessionId, send prompt |
| `relay_` | Forward information | Relay user feedback to worker |
| `add_` | Add label | Add the specified label to the issue |
| `remove_` | Remove label + retry | Remove label, then re-evaluate |
| `retry_` | Wait | Do nothing this iteration, re-check next loop |
| `skip` | No action needed | Check raw signals for edge cases (see signals table below) |
| `investigate_` | Anomaly detected | Log warning, inspect issue state manually |

This routing is stable across code changes. New action types automatically route
correctly if they follow the naming convention.
```

**Step 2: Commit**

```bash
jj describe -m "docs: replace controller transition table with intent-based routing"
jj new
```

---

### Task 17: Update architecture docs — honest framing

Update AGENTS.md and controller skill to reflect the actual architecture: the TS state machine provides deterministic suggestions + raw signals, the controller skill is the customization point, users modify the skill to change behavior.

**Files:**
- Modify: `AGENTS.md`
- Modify: `.claude/skills/legion-controller/SKILL.md`

**Step 1: Update AGENTS.md architecture description**

Replace the Two-Layer Architecture section to be honest about who owns what:

```markdown
## Architecture

The state machine provides **deterministic defaults + raw signals**. The controller skill
**decides whether to follow or override** them. Users customize behavior by modifying the
controller skill, not the TypeScript.

- **TypeScript daemon** — thin substrate: spawns processes, tracks health, computes
  deterministic session IDs, collects signals from Linear/GitHub/workers, suggests actions.
  The suggestions are testable defaults, not policy.
- **Controller skill** — the customization point: reads suggested actions + raw signals,
  executes transitions, runs quality gates, handles edge cases. Users who want different
  workflows edit this file.
- **Worker skills** — execute specific workflow phases (architect, plan, implement, review,
  retro, merge). Each is independently modifiable.
```

**Step 2: Update controller skill header**

Add a note at the top of SKILL.md:

```markdown
> **Customization:** This skill is the primary extension point for Legion's behavior.
> The state machine provides suggested actions and raw signals. This skill decides what
> to do with them. Modify this file to change how issues flow through the pipeline.
```

**Step 3: Commit**

```bash
jj describe -m "docs: honest architecture framing — TS provides defaults, skills own policy"
jj new
```

---

### Task 18: Final verification

Run the full test suite, type checker, and linter. Verify all regression tests pass.

**Files:** None (verification only)

**Step 1: Run all regression tests specifically**

Run: `bun test packages/daemon/src/state/__tests__/decision-regressions.test.ts packages/daemon/src/daemon/__tests__/session-id-contract.test.ts packages/opencode-plugin/src/tools/task/__tests__/storage-regressions.test.ts`
Expected: ALL PASS (these were failing before hardening)

**Step 2: Run full test suite**

Run: `bun test`
Expected: ALL PASS

**Step 3: Run type checker**

Run: `bunx tsc --noEmit`
Expected: No errors

**Step 4: Run linter**

Run: `bunx biome check src/ packages/`
Expected: No errors (or only pre-existing warnings)

**Step 5: Fix any issues found**

If any check fails, fix and re-run until all pass.

**Step 6: Final commit (if any fixes needed)**

```bash
jj describe -m "chore: fix lint/type issues from state machine hardening"
jj new
```
