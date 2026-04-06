# Envoy Subscription Lifecycle Management — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route Envoy events to the correct active worker per issue by adding mode-based subscription, cross-mode cleanup, topic tracking, and health tick safety-net cleanup.

**Architecture:** Only `plan` mode gets daemon-managed issue-topic subscriptions at dispatch; all other modes get none. A shared `detachWorkerFromEnvoy()` helper clears local `envoyTopics` first (idempotent), then fires async unsubscribe. Cross-mode cleanup on dispatch unsubscribes same-issue workers using exact issue ID parsing. DELETE always blanket-unsubscribes (preserving backward compat for worker self-managed PR subscriptions). Health tick reads the state file for dead workers with stale `envoyTopics` as a safety net.

**Tech Stack:** TypeScript on Bun, `bun test`, `bunx tsc --noEmit`, `bunx biome check`. jj for version control.

**Repo config:** No `.legion/config.yml` — defaults apply.

**Required Skills:**
| Phase | Skills |
|-------|--------|
| Implement | `envoy` |

---

## File Structure

| File | Responsibility | Change Type |
|------|---------------|-------------|
| `packages/daemon/src/daemon/server.ts` | HTTP handlers, Envoy helpers | Modify: new `detachWorkerFromEnvoy` helper, refactored `subscribeWorkerToEnvoy`, dispatch/delete/PATCH wiring |
| `packages/daemon/src/daemon/index.ts` | Daemon lifecycle, health tick | Modify: dead worker Envoy cleanup in health tick |
| `packages/daemon/src/daemon/__tests__/server.test.ts` | Server tests | Modify: updated + new tests for mode-based topics, cross-mode cleanup, lifecycle |
| `.opencode/skills/legion-worker/workflows/implement.md` | Implement workflow | Modify: add resume PR subscription |

**Not modified:** `packages/daemon/src/daemon/serve-manager.ts` — `envoyTopics?: string[]` already exists on `WorkerEntry` (added in a prior PR).

---

## Task 1: Mode-based subscription, cross-mode cleanup, handler updates, and tests — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts:141-186` (subscribe/unsubscribe helpers)
- Modify: `packages/daemon/src/daemon/server.ts:512-518` (POST /workers Envoy section)
- Modify: `packages/daemon/src/daemon/server.ts:660-732` (PATCH and DELETE handlers)
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts:1336-1581` (Envoy test section)

### Helpers

- [ ] **Step 1: Refactor `subscribeWorkerToEnvoy()` to accept a topics array**

Replace the existing `subscribeWorkerToEnvoy()` function in `packages/daemon/src/daemon/server.ts` (lines 141-167). The current signature is `(sessionId, owner, repo, issueNumber)`. Change it to accept pre-computed topics:

```typescript
function subscribeWorkerToEnvoy(sessionId: string, topics: string[]): void {
  if (topics.length === 0) return;
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/subscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      session_id: sessionId,
      topics,
    }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy worker subscribe returned ${res.status} for session=${sessionId} (non-fatal)`,
        );
      }
    })
    .catch((err) => {
      console.warn(
        `Envoy worker subscribe failed for session=${sessionId} (non-fatal): ${err}`,
      );
    });
}
```

- [ ] **Step 2: Replace `unsubscribeWorkerFromEnvoy()` with `detachWorkerFromEnvoy()`**

Replace the existing `unsubscribeWorkerFromEnvoy()` function (lines 169-186) with a helper that clears local `envoyTopics` first (making cleanup idempotent), then fires async unsubscribe:

```typescript
function detachWorkerFromEnvoy(entry: WorkerEntry, reason: string): void {
  const hadTopics = entry.envoyTopics;
  entry.envoyTopics = undefined;
  if (!hadTopics?.length) return;
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/unsubscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: entry.sessionId, topics: [] }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy worker unsubscribe (${reason}) returned ${res.status} for session=${entry.sessionId} (non-fatal)`,
        );
      }
    })
    .catch((err) => {
      console.warn(
        `Envoy worker unsubscribe (${reason}) failed for session=${entry.sessionId} (non-fatal): ${err}`,
      );
    });
}
```

Key design: clears `entry.envoyTopics` **before** firing async unsubscribe. This means a second call to `detachWorkerFromEnvoy` on the same entry is a no-op (idempotent). Used by dispatch cleanup, DELETE, PATCH-to-dead, and health tick.

- [ ] **Step 3: Also keep a blanket `unsubscribeWorkerFromEnvoy()` for DELETE**

DELETE must blanket-unsubscribe all topics (including worker self-managed PR subscriptions), not just daemon-managed ones. Keep the existing blanket function, renamed for clarity:

```typescript
function unsubscribeAllWorkerTopics(sessionId: string): void {
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/unsubscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: sessionId, topics: [] }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy worker unsubscribe returned ${res.status} for session=${sessionId} (non-fatal)`,
        );
      }
    })
    .catch((err) => {
      console.warn(
        `Envoy worker unsubscribe failed for session=${sessionId} (non-fatal): ${err}`,
      );
    });
}
```

### Dispatch Handler

- [ ] **Step 4: Wire cross-mode cleanup and mode-based subscription in POST /workers**

Replace lines 512-518 of `packages/daemon/src/daemon/server.ts` (the existing Envoy auto-subscribe block) with:

```typescript
            // Cross-mode cleanup: unsubscribe same-issue workers from Envoy
            for (const [existingId, existingEntry] of workers) {
              if (existingId === workerId) continue;
              const existingIssueId = extractIssueIdFromWorkerId(existingId);
              if (existingIssueId === normalizedIssueId) {
                detachWorkerFromEnvoy(existingEntry, "cross-mode-cleanup");
              }
            }

            // Mode-based Envoy subscription (GitHub-only, fire-and-forget)
            // Only plan mode subscribes to issue topics at dispatch.
            // Implement self-subscribes to PR topics after PR creation via envoy_subscribe.
            if (
              mode === WorkerMode.PLAN &&
              typeof repo === "string" &&
              issueNumber !== undefined
            ) {
              const repoRef = parseIssueRepo(repo);
              if (repoRef) {
                const topics = [
                  `notifications.github.${repoRef.owner}.${repoRef.repo}.issue.${issueNumber}.>`,
                ];
                subscribeWorkerToEnvoy(actualSessionId, topics);
                entry.envoyTopics = topics;
              }
            }
```

Note: `normalizedIssueId` is already in scope (line 355). No need to re-derive from `workerId`. `extractIssueIdFromWorkerId` is used on `existingId` for exact matching (avoids prefix collision between e.g. `eng-1` and `eng-10`).

### DELETE Handler

- [ ] **Step 5: Update DELETE handler**

Replace lines 729-732 in the DELETE handler:

```typescript
// FROM:
workers.delete(id);
await persistState();
unsubscribeWorkerFromEnvoy(entry.sessionId);
return jsonResponse({ status: "stopped" });
```

WITH:

```typescript
            entry.envoyTopics = undefined;
            workers.delete(id);
            await persistState();
            unsubscribeAllWorkerTopics(entry.sessionId);
            return jsonResponse({ status: "stopped" });
```

Note: `entry.envoyTopics = undefined` clears tracking. `unsubscribeAllWorkerTopics` does blanket unsubscribe of all topics (both daemon-managed and worker self-managed). This preserves backward compat.

### PATCH Handler

- [ ] **Step 6: Add cleanup in PATCH handler when status transitions to dead**

In the PATCH handler (around lines 660-723), after the existing dead-status crash count logic (around line 695) and before `await persistState()` (line 705), add:

```typescript
            if (updated.status === "dead" && entry.status !== "dead") {
              detachWorkerFromEnvoy(updated, "worker-dead");
            }
```

This clears `envoyTopics` and fires unsubscribe when a worker transitions to dead. The `detachWorkerFromEnvoy` helper clears local state first, so a subsequent health tick cleanup is a no-op.

### Tests

- [ ] **Step 7: Update existing test — plan mode subscribes**

Update the test at line 1383 of `packages/daemon/src/daemon/__tests__/server.test.ts`. Change mode from `"implement"` to `"plan"` since only plan subscribes:

```typescript
it("subscribes worker to Envoy issue topic when mode is plan", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({ paths: repoPaths, repoManagerDeps });

  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-42",
      mode: "plan",
      repo: "acme/widgets",
      issueNumber: 42,
    }),
  });

  expect(response.status).toBe(200);
  const body = (await response.json()) as { sessionId: string };

  await Bun.sleep(50);

  expect(envoySubscribeCalls).toHaveLength(1);
  expect(envoySubscribeCalls[0].body.session_id).toBe(body.sessionId);
  expect(envoySubscribeCalls[0].body.topics).toEqual([
    "notifications.github.acme.widgets.issue.42.>",
  ]);
});
```

- [ ] **Step 8: Add table-driven tests — non-plan modes do NOT subscribe**

```typescript
for (const mode of ["implement", "test", "review", "architect", "merge"]) {
  it(`does not subscribe ${mode} worker to Envoy at dispatch`, async () => {
    const envoySubscribeCalls: EnvoySubscribeCall[] = [];
    mockFetchForEnvoy(envoySubscribeCalls);

    await startTestServer({ paths: repoPaths, repoManagerDeps });

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: `acme-widgets-${mode}`,
        mode,
        repo: "acme/widgets",
        issueNumber: 42,
      }),
    });

    expect(response.status).toBe(200);
    await Bun.sleep(50);

    const subscribeCalls = envoySubscribeCalls.filter((c) =>
      c.url.includes("/v1/interests/subscribe"),
    );
    expect(subscribeCalls).toHaveLength(0);
  });
}
```

- [ ] **Step 9: Add test — cross-mode cleanup: dispatch implement unsubscribes plan**

```typescript
it("unsubscribes same-issue plan worker when implement is dispatched", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({ paths: repoPaths, repoManagerDeps });

  // Dispatch plan worker first
  const planResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-50",
      mode: "plan",
      repo: "acme/widgets",
      issueNumber: 50,
    }),
  });
  expect(planResponse.status).toBe(200);
  const planBody = (await planResponse.json()) as { sessionId: string };
  await Bun.sleep(50);
  envoySubscribeCalls.length = 0;

  // Dispatch implement worker for the same issue
  const implResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-50",
      mode: "implement",
      repo: "acme/widgets",
      issueNumber: 50,
    }),
  });
  expect(implResponse.status).toBe(200);
  await Bun.sleep(50);

  // Plan worker was unsubscribed
  const unsubCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/unsubscribe"),
  );
  expect(unsubCalls).toHaveLength(1);
  expect(unsubCalls[0].body.session_id).toBe(planBody.sessionId);

  // Implement worker was NOT subscribed
  const subCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/subscribe"),
  );
  expect(subCalls).toHaveLength(0);
});
```

- [ ] **Step 10: Add test — cross-mode cleanup does NOT affect different issue**

```typescript
it("does not unsubscribe workers for a different issue on cross-mode dispatch", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({ paths: repoPaths, repoManagerDeps });

  // Dispatch plan worker for issue 50
  await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-50",
      mode: "plan",
      repo: "acme/widgets",
      issueNumber: 50,
    }),
  });
  await Bun.sleep(50);
  envoySubscribeCalls.length = 0;

  // Dispatch implement worker for issue 51 (different issue)
  await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-51",
      mode: "implement",
      repo: "acme/widgets",
      issueNumber: 51,
    }),
  });
  await Bun.sleep(50);

  // No unsubscribe calls — issue 50's plan worker untouched
  const unsubCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/unsubscribe"),
  );
  expect(unsubCalls).toHaveLength(0);
});
```

- [ ] **Step 11: Add test — envoyTopics in GET /workers response**

```typescript
it("includes envoyTopics in GET /workers for plan, undefined for implement", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({ paths: repoPaths, repoManagerDeps });

  await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-60",
      mode: "plan",
      repo: "acme/widgets",
      issueNumber: 60,
    }),
  });
  await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-61",
      mode: "implement",
      repo: "acme/widgets",
      issueNumber: 61,
    }),
  });
  await Bun.sleep(50);

  const listResponse = await requestJson("/workers", { method: "GET" });
  const workers = (await listResponse.json()) as Array<{
    id: string;
    envoyTopics?: string[];
  }>;

  const planWorker = workers.find((w) => w.id === "acme-widgets-60-plan");
  expect(planWorker?.envoyTopics).toEqual([
    "notifications.github.acme.widgets.issue.60.>",
  ]);

  const implWorker = workers.find((w) => w.id === "acme-widgets-61-implement");
  expect(implWorker?.envoyTopics).toBeUndefined();
});
```

- [ ] **Step 12: Update existing DELETE test to use plan mode**

Update the existing "unsubscribes worker from envoy on delete" test (line 1523) to dispatch a `plan` worker instead of `implement`, since that's the mode that gets daemon-managed subscriptions:

```typescript
it("clears envoyTopics and unsubscribes from envoy on delete", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({ paths: repoPaths, repoManagerDeps });

  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-250",
      mode: "plan",
      repo: "acme/widgets",
      issueNumber: 250,
    }),
  });
  expect(createResponse.status).toBe(200);
  const created = (await createResponse.json()) as {
    id: string;
    sessionId: string;
  };
  await Bun.sleep(50);
  envoySubscribeCalls.length = 0;

  const deleteResponse = await requestJson(`/workers/${created.id}`, {
    method: "DELETE",
  });
  expect(deleteResponse.status).toBe(200);
  await Bun.sleep(50);

  const unsubCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/unsubscribe"),
  );
  expect(unsubCalls).toHaveLength(1);
  expect(unsubCalls[0].body).toEqual({
    session_id: created.sessionId,
    topics: [],
  });
});
```

- [ ] **Step 13: Add test — PATCH to dead clears envoyTopics and fires unsubscribe**

```typescript
it("clears envoyTopics when worker status is patched to dead", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({ paths: repoPaths, repoManagerDeps });

  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-70",
      mode: "plan",
      repo: "acme/widgets",
      issueNumber: 70,
    }),
  });
  expect(createResponse.status).toBe(200);
  const created = (await createResponse.json()) as {
    id: string;
    sessionId: string;
  };
  await Bun.sleep(50);
  envoySubscribeCalls.length = 0;

  const patchResponse = await requestJson(`/workers/${created.id}`, {
    method: "PATCH",
    body: JSON.stringify({ status: "dead" }),
  });
  expect(patchResponse.status).toBe(200);
  await Bun.sleep(50);

  const unsubCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/unsubscribe"),
  );
  expect(unsubCalls).toHaveLength(1);
  expect(unsubCalls[0].body.session_id).toBe(created.sessionId);

  // envoyTopics cleared
  const listResponse = await requestJson("/workers", { method: "GET" });
  const workers = (await listResponse.json()) as Array<{
    id: string;
    envoyTopics?: string[];
  }>;
  expect(workers.find((w) => w.id === created.id)?.envoyTopics).toBeUndefined();
});
```

### Verification

- [ ] **Step 14: Run type check**

Run: `bunx tsc --noEmit`
Expected: Clean — no errors.

- [ ] **Step 15: Run tests**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: ALL tests pass — both new and existing. Check that existing tests that previously dispatched `implement` and expected subscribe calls are updated to use `plan`.

- [ ] **Step 16: Run lint**

Run: `bunx biome check src/`
Expected: Clean.

- [ ] **Step 17: Commit**

```bash
jj describe -m "feat: add mode-based envoy subscription lifecycle management

Only plan mode subscribes to issue topics at dispatch. Cross-mode cleanup
unsubscribes same-issue workers using exact issue ID matching. DELETE
blanket-unsubscribes all topics. PATCH-to-dead clears daemon-managed
subscriptions via shared detachWorkerFromEnvoy helper."
jj new
```

---

## Task 2: Health tick dead worker cleanup — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts:663-673` (health tick)

The health tick reads the state file (which includes `envoyTopics` after `persistState()` in server.ts). For dead workers with stale `envoyTopics`, call the existing `unsubscribeFromEnvoy()` in index.ts and update the state file. This avoids expanding the `startServer()` API.

- [ ] **Step 1: Add dead worker Envoy cleanup to health tick**

In `packages/daemon/src/daemon/index.ts`, in the health tick section, add after the `feedbackLogger?.log` block (around line 673) and before the `finally` block (line 674):

```typescript
        // Cleanup dead worker Envoy subscriptions (safety net for missed PATCH-to-dead)
        let envoyCleanupCount = 0;
        for (const entry of Object.values(workerState.workers)) {
          if (entry.status === "dead" && entry.envoyTopics?.length) {
            unsubscribeFromEnvoy(entry.sessionId);
            entry.envoyTopics = undefined;
            envoyCleanupCount += 1;
          }
        }
        if (envoyCleanupCount > 0) {
          await resolvedDeps.writeStateFile(config.stateFilePath, workerState);
        }
```

Note: `workerState` is already read at line 664 (`const workerState = await resolvedDeps.readStateFile(config.stateFilePath)`). The existing `unsubscribeFromEnvoy()` (line 160) is already in index.ts and follows the fire-and-forget pattern. Writing the state file after clearing `envoyTopics` prevents the next health tick from re-unsubscribing the same dead workers.

- [ ] **Step 2: Run type check**

Run: `bunx tsc --noEmit`
Expected: Clean.

- [ ] **Step 3: Run full test suite**

Run: `bun test`
Expected: All 640+ tests pass. No regressions from the health tick change.

- [ ] **Step 4: Amend into previous commit (same logical feature)**

This is a small addition to the same feature. Fold it into the previous commit:

```bash
jj squash
```

If the implementer prefers a separate commit:

```bash
jj describe -m "feat: add health tick cleanup for dead worker envoy subscriptions

Safety net: health tick reads state file, finds dead workers with stale
envoyTopics, unsubscribes and clears them. Handles edge cases where
PATCH-to-dead didn't fire (e.g., worker died without status update)."
jj new
```

---

## Task 3: Update implement workflow for resume PR subscription — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/implement.md:31-59` (startup section)

- [ ] **Step 1: Add resume PR subscription check**

In `.opencode/skills/legion-worker/workflows/implement.md`, after the "All Modes: Rebase First" section (line 38, after "Resolve any conflicts before proceeding.") and before section "1.2. Load Repo Config" (line 40), add:

```markdown
### 0.5. Resume: Subscribe to Existing PR Events

When resumed (not freshly dispatched), check if a PR already exists for this branch and subscribe to its Envoy events. This ensures the implementer receives PR comments, reviews, and CI events on resume, not only on initial PR creation.

```bash
PR_NUMBER=$(gh pr view "$LEGION_ISSUE_ID" --json number --jq '.number' -R $OWNER/$REPO 2>/dev/null || echo "")
```

If `PR_NUMBER` is non-empty, subscribe:

```
envoy_subscribe(["notifications.github.$OWNER.$REPO.pr.$PR_NUMBER.>"])
```

**If `envoy_subscribe` fails:** Log and continue — this is a speed optimization, not a requirement. The controller's polling cycle is the authoritative fallback.

**If no PR exists:** Skip silently — this is a fresh dispatch and step 6.1 handles subscription after PR creation.
```

- [ ] **Step 2: Verify step 6.1 is consistent**

Read step 6.1 ("Subscribe to PR Topics (Envoy)") at line 306-321. Confirm it covers the initial PR creation case with `envoy_subscribe` after `gh pr create`. No changes needed — it already matches.

- [ ] **Step 3: Commit**

```bash
jj describe -m "docs: add resume PR subscription to implement workflow

When an implement worker is resumed, check for existing PR and subscribe
to its Envoy events. Complements step 6.1 (initial PR creation)."
jj new
```

---

## Task 4: Final verification — Depends on: Task 1, Task 2, Task 3

**Files:** None (verification only)

- [ ] **Step 1: Run full test suite**

Run: `bun test`
Expected: All 640+ tests pass.

- [ ] **Step 2: Run type check**

Run: `bunx tsc --noEmit`
Expected: Clean.

- [ ] **Step 3: Run lint**

Run: `bunx biome check src/`
Expected: Clean (or only pre-existing warnings).

- [ ] **Step 4: Verify acceptance criteria**

Verify each criterion from the issue:

1. ✅ When `{issue}-implement` dispatched, `{issue}-plan` unsubscribed (cross-mode cleanup test)
2. ✅ When `{issue}-implement` dispatched, no subscribe call (table-driven mode test)
3. ✅ When `{issue}-plan` dispatched, subscribe with correct topic (plan subscribe test)
4. ✅ test/review/architect/merge do not trigger subscribe (table-driven mode test)
5. ✅ GET /workers includes envoyTopics (GET response test)
6. ✅ DELETE calls unsubscribe and clears envoyTopics (DELETE test)
7. ✅ Health tick cleans dead workers with envoyTopics (health tick in index.ts)
8. ✅ Dispatch/delete not blocked by subscribe/unsubscribe (fire-and-forget pattern)
9. ✅ Failures logged at warn without throwing (existing pattern preserved)
10. ✅ Implement workflow has envoy_subscribe for PR on creation and resume (steps 0.5 + 6.1)

---

## Testing Plan

### Setup
- `bun install` (if not already done)
- No running servers needed — all tests use mocked fetch and in-memory workers

### Health Check
- `bun test packages/daemon/src/daemon/__tests__/server.test.ts --timeout 30000`
- Expected: all tests pass within 30 seconds

### Verification Steps

For each acceptance criterion:

1. **Plan mode subscribes to issue topic**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "subscribes worker to Envoy issue topic when mode is plan"`
   - Expected: PASS — subscribe call with `notifications.github.acme.widgets.issue.42.>` topic
   - Tool: bun test

2. **Non-plan modes do not subscribe**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "does not subscribe"`
   - Expected: PASS for implement, test, review, architect, merge
   - Tool: bun test

3. **Cross-mode cleanup**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "unsubscribes same-issue plan worker when implement is dispatched"`
   - Expected: PASS — unsubscribe called with plan's session ID
   - Tool: bun test

4. **Different issue not affected**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "does not unsubscribe workers for a different issue"`
   - Expected: PASS — no unsubscribe calls
   - Tool: bun test

5. **GET /workers envoyTopics**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "includes envoyTopics"`
   - Expected: PASS — plan has topics, implement has undefined
   - Tool: bun test

6. **DELETE lifecycle**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "clears envoyTopics and unsubscribes"`
   - Expected: PASS — unsubscribe called, topics cleared
   - Tool: bun test

7. **PATCH-to-dead lifecycle**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "clears envoyTopics when worker status is patched to dead"`
   - Expected: PASS — unsubscribe called, topics cleared
   - Tool: bun test

8. **Type check**
   - Action: `bunx tsc --noEmit`
   - Expected: Exit code 0
   - Tool: tsc

9. **Full test suite**
   - Action: `bun test`
   - Expected: All 640+ tests pass
   - Tool: bun test

### Tools Needed
- `bun test` — unit test runner with fetch mocking
- `bunx tsc --noEmit` — TypeScript type checker
- `bunx biome check src/` — linter
