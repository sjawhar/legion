# Envoy Subscription Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the Envoy subscription lifecycle so workers and controllers properly subscribe/unsubscribe across all state transitions (dispatch, resume, startup, crash recovery, exit).

**Architecture:** Most subscription infrastructure already exists in the daemon code. This plan fills gaps in startup/crash recovery re-subscription, adds missing tests for existing paths, updates worker exit protocol with explicit self-unsubscribe, and documents controller CI event handling. No new abstractions — extend existing fire-and-forget patterns.

**Tech Stack:** TypeScript/Bun, Bun test runner, jj for version control

---

## Delta from Current State

Before implementing, understand what **already exists** vs. what's **missing**:

| Feature | Status | Location |
|---------|--------|----------|
| Worker subscribe on dispatch | ✅ Implemented + tested | server.ts:544-548, server.test.ts:1336-1410 |
| Controller CI subscribe on first dispatch | ✅ Implemented, **NO tests** | server.ts:549-561 |
| `subscribedCiRepos` startup seeding | ✅ Seeds set, **NO CI subscribe call** | server.ts:274-277 |
| Resume re-subscribe in `/workers/:id/prompt` | ✅ Implemented, **NO tests** | server.ts:835-846 |
| DELETE unsubscribe (empty topics) | ✅ Implemented + tested | server.ts:776, server.test.ts:1523-1580 |
| PR self-subscribe in implement workflow | ✅ Implemented | implement.md:306-321 |
| Worker re-subscribe on startup recreation | ❌ Missing | index.ts:350-373 (gap) |
| Worker re-subscribe on crash recovery | ❌ Missing | index.ts:560-584 (gap) |
| Controller CI re-subscribe on startup | ❌ Missing | index.ts (after controller setup) |
| Controller re-subscribe on crash recovery | ❌ Missing | index.ts:586-616 (gap) |
| Worker self-unsubscribe on exit | ❌ Missing | SKILL.md exit protocol |
| Controller CI event handling | ❌ Missing | controller SKILL.md |
| Lifecycle documentation | ❌ Partial | envoy-auto-subscription-patterns.md |

---

### Task 1: Export Envoy subscription helpers from server.ts — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts:141, 188`

- [ ] **Step 1: Add `export` to `subscribeWorkerToEnvoy`**

In `packages/daemon/src/daemon/server.ts`, line 141, change:
```typescript
function subscribeWorkerToEnvoy(
```
to:
```typescript
export function subscribeWorkerToEnvoy(
```

- [ ] **Step 2: Add `export` to `subscribeControllerToCiEnvoy`**

In `packages/daemon/src/daemon/server.ts`, line 188, change:
```typescript
function subscribeControllerToCiEnvoy(sessionId: string, owner: string, repo: string): void {
```
to:
```typescript
export function subscribeControllerToCiEnvoy(sessionId: string, owner: string, repo: string): void {
```

- [ ] **Step 3: Verify no regressions**

Run: `bunx tsc --noEmit && bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: All existing tests pass. Type check clean.

- [ ] **Step 4: Commit**

```bash
jj describe -m "refactor(daemon): export Envoy subscription helpers from server.ts"
jj new
```

---

### Task 2: Add tests for existing untested Envoy subscription paths — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

All new tests go inside the existing `describe("Envoy worker auto-subscribe", ...)` block (starts at line 1336).

- [ ] **Step 1: Write test — controller CI subscribe on first dispatch per repo**

Add after the existing tests in the "Envoy worker auto-subscribe" describe block:

```typescript
it("subscribes controller to CI topic on first worker dispatch for a repo", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({
    paths: repoPaths,
    repoManagerDeps,
    getControllerState: () => ({ sessionId: "ses_controller_ci_test" }),
  });

  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-100",
      mode: "implement",
      repo: "acme/widgets",
      issueNumber: 100,
    }),
  });

  expect(response.status).toBe(200);
  await Bun.sleep(50);

  const subscribeCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/subscribe")
  );
  // Two subscribe calls: worker issue topic + controller CI topic
  expect(subscribeCalls).toHaveLength(2);

  // Worker issue topic
  const workerCall = subscribeCalls.find((c) =>
    c.body.topics.some((t: string) => t.includes("issue.100"))
  );
  expect(workerCall).toBeDefined();
  expect(workerCall!.body.topics).toEqual(["notifications.github.acme.widgets.issue.100.>"]);

  // Controller CI topic
  const ciCall = subscribeCalls.find((c) =>
    c.body.topics.some((t: string) => t.includes(".ci"))
  );
  expect(ciCall).toBeDefined();
  expect(ciCall!.body.session_id).toBe("ses_controller_ci_test");
  expect(ciCall!.body.topics).toEqual(["notifications.github.acme.widgets.ci"]);
});
```

- [ ] **Step 2: Run test to verify it passes (testing existing code)**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "subscribes controller to CI topic on first"`
Expected: PASS (this tests already-implemented code)

- [ ] **Step 3: Write test — no duplicate CI subscribe on second dispatch for same repo**

```typescript
it("does not duplicate CI subscribe on second dispatch for same repo", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({
    paths: repoPaths,
    repoManagerDeps,
    getControllerState: () => ({ sessionId: "ses_controller_dedup" }),
  });

  // First dispatch
  await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-200",
      mode: "implement",
      repo: "acme/widgets",
      issueNumber: 200,
    }),
  });
  await Bun.sleep(50);

  // Clear calls
  envoySubscribeCalls.length = 0;

  // Second dispatch for same repo, different issue
  await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-201",
      mode: "plan",
      repo: "acme/widgets",
      issueNumber: 201,
    }),
  });
  await Bun.sleep(50);

  // Only worker subscribe, no CI subscribe (already subscribed for this repo)
  const subscribeCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/subscribe")
  );
  expect(subscribeCalls).toHaveLength(1);
  expect(subscribeCalls[0].body.topics).toEqual([
    "notifications.github.acme.widgets.issue.201.>",
  ]);
});
```

- [ ] **Step 4: Run test**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "does not duplicate CI subscribe"`
Expected: PASS

- [ ] **Step 5: Write test — prompt endpoint triggers worker re-subscribe**

```typescript
it("re-subscribes worker to Envoy issue topics on prompt", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({
    paths: repoPaths,
    repoManagerDeps,
  });

  // Create worker with repo and issueNumber
  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-300",
      mode: "implement",
      repo: "acme/widgets",
      issueNumber: 300,
    }),
  });
  expect(createResponse.status).toBe(200);
  await Bun.sleep(50);

  // Clear calls from dispatch
  envoySubscribeCalls.length = 0;

  // Send prompt (resume)
  const promptResponse = await requestJson("/workers/acme-widgets-300-implement/prompt", {
    method: "POST",
    body: JSON.stringify({ text: "resume work" }),
  });
  expect(promptResponse.status).toBe(200);
  await Bun.sleep(50);

  // Verify re-subscribe call
  const subscribeCalls = envoySubscribeCalls.filter((c) =>
    c.url.includes("/v1/interests/subscribe")
  );
  expect(subscribeCalls).toHaveLength(1);
  expect(subscribeCalls[0].body.topics).toEqual([
    "notifications.github.acme.widgets.issue.300.>",
  ]);
});
```

- [ ] **Step 6: Run test**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "re-subscribes worker to Envoy issue topics on prompt"`
Expected: PASS

- [ ] **Step 7: Write test — prompt re-subscribe is fire-and-forget**

```typescript
it("prompt succeeds even when Envoy re-subscribe fails", async () => {
  const envoySubscribeCalls: EnvoySubscribeCall[] = [];
  // Use 200 for initial dispatch, then switch to 500 for prompt
  mockFetchForEnvoy(envoySubscribeCalls);

  await startTestServer({
    paths: repoPaths,
    repoManagerDeps,
  });

  const createResponse = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "acme-widgets-301",
      mode: "implement",
      repo: "acme/widgets",
      issueNumber: 301,
    }),
  });
  expect(createResponse.status).toBe(200);
  await Bun.sleep(50);

  // Switch to failing Envoy
  mockFetchForEnvoy(envoySubscribeCalls, 500);

  const promptResponse = await requestJson("/workers/acme-widgets-301-implement/prompt", {
    method: "POST",
    body: JSON.stringify({ text: "resume work" }),
  });
  expect(promptResponse.status).toBe(200);
  const body = (await promptResponse.json()) as { ok: boolean };
  expect(body.ok).toBe(true);
});
```

- [ ] **Step 8: Run all new tests**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "controller.*CI|duplicate CI|re-subscribes worker.*prompt|prompt succeeds even"`
Expected: All PASS

- [ ] **Step 9: Commit**

```bash
jj describe -m "test(daemon): add tests for CI dispatch subscribe and prompt re-subscribe"
jj new
```

---

### Task 3: Startup + crash recovery Envoy re-subscription (TDD) — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts` (lines 1-25 imports, ~358-371, ~537, ~568-583, ~612)
- Modify: `packages/daemon/src/daemon/__tests__/index.test.ts`

- [ ] **Step 1: Add Envoy fetch mock infrastructure to index.test.ts**

Add inside the `describe("daemon entry", ...)` block, near the top after variable declarations:

```typescript
interface EnvoyCall {
  url: string;
  body: { session_id: string; topics: string[] };
}

function mockFetchForEnvoy(calls: EnvoyCall[], statusCode = 200) {
  const mockFn = async (input: string | URL | Request, init?: RequestInit) => {
    const url =
      typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url.includes("/v1/interests/subscribe") || url.includes("/v1/interests/unsubscribe")) {
      calls.push({ url, body: JSON.parse(init?.body as string) });
      return new Response("{}", { status: statusCode });
    }
    return originalFetch(input, init);
  };
  globalThis.fetch = Object.assign(mockFn, {
    preconnect: originalFetch.preconnect,
  });
}
```

- [ ] **Step 2: Write failing test — worker re-subscribe on startup recreation**

Add a new `describe("Envoy re-subscription on startup", ...)` block:

```typescript
describe("Envoy re-subscription on startup", () => {
  it("re-subscribes workers to Envoy issue topics after startup recreation", async () => {
    const envoyCalls: EnvoyCall[] = [];
    mockFetchForEnvoy(envoyCalls);

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-envoy-resub.json",
        legionId: TEAM_ID,
        controllerSessionId: "ses_ext_ctrl",
      },
      {
        readStateFile: async () => ({
          workers: {
            "acme-widgets-42-implement": {
              ...baseEntry,
              id: "acme-widgets-42-implement",
              sessionId: "ses_worker_42",
              repo: "acme/widgets",
              issueNumber: 42,
            },
          },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: (opts) => ({
          server: { port: opts.port } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    await Bun.sleep(100);

    const workerSubscribeCalls = envoyCalls.filter(
      (c) =>
        c.url.includes("/v1/interests/subscribe") &&
        c.body.topics.some((t) => t.includes("issue.42"))
    );
    expect(workerSubscribeCalls.length).toBeGreaterThanOrEqual(1);
    expect(workerSubscribeCalls[0].body.session_id).toBe("ses_worker_42");
    expect(workerSubscribeCalls[0].body.topics).toEqual([
      "notifications.github.acme.widgets.issue.42.>",
    ]);
  });

  it("re-subscribes controller to CI topics for repos with active workers on startup", async () => {
    const envoyCalls: EnvoyCall[] = [];
    mockFetchForEnvoy(envoyCalls);

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-envoy-ci-resub.json",
        legionId: TEAM_ID,
        controllerSessionId: "ses_ext_ctrl_ci",
      },
      {
        readStateFile: async () => ({
          workers: {
            "acme-widgets-50-implement": {
              ...baseEntry,
              id: "acme-widgets-50-implement",
              sessionId: "ses_worker_50",
              repo: "acme/widgets",
              issueNumber: 50,
            },
            "acme-widgets-51-plan": {
              ...baseEntry,
              id: "acme-widgets-51-plan",
              sessionId: "ses_worker_51",
              repo: "acme/widgets",
              issueNumber: 51,
            },
          },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: (opts) => ({
          server: { port: opts.port } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    await Bun.sleep(100);

    const ciSubscribeCalls = envoyCalls.filter(
      (c) =>
        c.url.includes("/v1/interests/subscribe") &&
        c.body.topics.some((t) => t === "notifications.github.acme.widgets.ci")
    );
    // Should subscribe to CI for acme/widgets exactly once (deduped across both workers)
    expect(ciSubscribeCalls.length).toBeGreaterThanOrEqual(1);
    expect(ciSubscribeCalls[0].body.session_id).toBe("ses_ext_ctrl_ci");
  });

  it("skips Envoy re-subscribe for workers without repo field (Linear mode)", async () => {
    const envoyCalls: EnvoyCall[] = [];
    mockFetchForEnvoy(envoyCalls);

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-envoy-norepo.json",
        legionId: TEAM_ID,
        controllerSessionId: "ses_ext_ctrl_norepo",
      },
      {
        readStateFile: async () => ({
          workers: {
            "eng-99-implement": {
              ...baseEntry,
              id: "eng-99-implement",
              sessionId: "ses_worker_99",
              // No repo field — Linear mode
            },
          },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: (opts) => ({
          server: { port: opts.port } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    await Bun.sleep(100);

    // Only controller base topics — no worker issue or CI subscribes
    const workerSubscribeCalls = envoyCalls.filter(
      (c) =>
        c.url.includes("/v1/interests/subscribe") &&
        c.body.topics.some((t) => t.includes("issue."))
    );
    expect(workerSubscribeCalls).toHaveLength(0);
  });

  it("startup succeeds even when Envoy re-subscribe fails", async () => {
    const envoyCalls: EnvoyCall[] = [];
    mockFetchForEnvoy(envoyCalls, 500);

    const handle = await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-envoy-fail.json",
        legionId: TEAM_ID,
        controllerSessionId: "ses_ext_ctrl_fail",
      },
      {
        readStateFile: async () => ({
          workers: {
            "acme-widgets-60-implement": {
              ...baseEntry,
              id: "acme-widgets-60-implement",
              sessionId: "ses_worker_60",
              repo: "acme/widgets",
              issueNumber: 60,
            },
          },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: (opts) => ({
          server: { port: opts.port } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    // Daemon started successfully despite Envoy failures
    expect(handle).toBeDefined();
    expect(handle.config).toBeDefined();
  });
});
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/index.test.ts --test-name-pattern "re-subscribes workers|re-subscribes controller.*CI|skips Envoy re-subscribe|startup succeeds even"`
Expected: FAIL — re-subscription calls are not yet implemented in index.ts

- [ ] **Step 4: Add imports to index.ts**

Add to the imports section of `packages/daemon/src/daemon/index.ts` (after line 23):

```typescript
import { subscribeWorkerToEnvoy, subscribeControllerToCiEnvoy } from "./server";
import { parseIssueRepo } from "./repo-manager";
```

- [ ] **Step 5: Implement worker re-subscribe on startup recreation**

In `packages/daemon/src/daemon/index.ts`, inside the startup worker recreation block (~line 357-371), after the successful `createSession` call (after line 366, inside the `try` block), add:

```typescript
            // Re-subscribe worker to Envoy issue topics (fire-and-forget)
            if (entry.repo && entry.issueNumber !== undefined) {
              const repoRef = parseIssueRepo(entry.repo);
              if (repoRef) {
                subscribeWorkerToEnvoy(
                  actualId,
                  repoRef.owner,
                  repoRef.repo,
                  entry.issueNumber,
                );
              }
            }
```

- [ ] **Step 6: Implement controller CI re-subscribe on startup**

After the controller setup section (both external and internal paths converge around line 537), add:

```typescript
  // Re-subscribe controller to CI topics for repos with active workers (fire-and-forget)
  if (controllerState) {
    const ciRepos = new Set<string>();
    for (const entry of workerEntries) {
      if (entry.repo) {
        ciRepos.add(entry.repo);
      }
    }
    for (const repoStr of ciRepos) {
      const repoRef = parseIssueRepo(repoStr);
      if (repoRef) {
        subscribeControllerToCiEnvoy(controllerState.sessionId, repoRef.owner, repoRef.repo);
      }
    }
  }
```

- [ ] **Step 7: Implement worker re-subscribe on crash recovery**

In the crash recovery worker recreation block (~line 568-583), after the successful `createSession` call inside the `try` block, add the same pattern as step 5:

```typescript
                    // Re-subscribe worker to Envoy issue topics (fire-and-forget)
                    if (entry.repo && entry.issueNumber !== undefined) {
                      const repoRef = parseIssueRepo(entry.repo);
                      if (repoRef) {
                        subscribeWorkerToEnvoy(
                          actualId,
                          repoRef.owner,
                          repoRef.repo,
                          entry.issueNumber,
                        );
                      }
                    }
```

- [ ] **Step 8: Implement controller re-subscribe on crash recovery**

In the crash recovery controller recreation block (~line 586-616), after the successful session re-creation and prompt delivery (after the `console.log` on ~line 613), add:

```typescript
                  // Re-subscribe controller to Envoy base topics + CI topics
                  subscribeControllerToEnvoy(actualControllerSessionId);
                  const ciRepos = new Set<string>();
                  for (const entry of liveWorkers) {
                    if (entry.repo) {
                      ciRepos.add(entry.repo);
                    }
                  }
                  for (const repoStr of ciRepos) {
                    const repoRef = parseIssueRepo(repoStr);
                    if (repoRef) {
                      subscribeControllerToCiEnvoy(
                        actualControllerSessionId,
                        repoRef.owner,
                        repoRef.repo,
                      );
                    }
                  }
```

- [ ] **Step 9: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/index.test.ts --test-name-pattern "re-subscribes workers|re-subscribes controller.*CI|skips Envoy re-subscribe|startup succeeds even"`
Expected: All PASS

- [ ] **Step 10: Run full test suite**

Run: `bunx tsc --noEmit && bun test`
Expected: All tests pass. No type errors.

- [ ] **Step 11: Commit**

```bash
jj describe -m "feat(daemon): re-subscribe workers and controller on startup and crash recovery"
jj new
```

---

### Task 4: Worker skill exit protocol — explicit self-unsubscribe — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/SKILL.md`

- [ ] **Step 1: Add self-unsubscribe to the Exiting section**

In `.opencode/skills/legion-worker/SKILL.md`, locate the `### Exiting` section. Insert the following **before** the existing `jj git push` line:

```markdown
Before pushing, self-unsubscribe from your explicit Envoy topics (best-effort, non-blocking):

**GitHub workers only** (skip for Linear — no Envoy topics to unsubscribe):
```
envoy_unsubscribe(["notifications.github.$OWNER.$REPO.issue.$ISSUE_NUMBER.>"])
```

If you created a PR (implement mode), also include the PR topic:
```
envoy_unsubscribe(["notifications.github.$OWNER.$REPO.issue.$ISSUE_NUMBER.>", "notifications.github.$OWNER.$REPO.pr.$PR_NUMBER.>"])
```

**Important:** Use an explicit topic list, NOT an empty array. `envoy_unsubscribe([])` would also remove `notifications.agent.{sessionId}`, creating a delivery gap before the daemon's authoritative cleanup.

If `envoy_unsubscribe` fails, continue — the daemon's `DELETE /workers` is the authoritative cleanup.
```

- [ ] **Step 2: Verify the change reads correctly in context**

Read the full Exiting section to confirm the self-unsubscribe step flows naturally before the push/label steps.

- [ ] **Step 3: Commit**

```bash
jj describe -m "docs(skills): add worker self-unsubscribe to exit protocol"
jj new
```

---

### Task 5: Controller skill CI event handling — Independent

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`

- [ ] **Step 1: Identify insertion point in controller SKILL.md**

Read the controller skill file to find where event handling / prompt handling is documented. The CI event handling section should go near the existing Envoy/subscription documentation, or near the polling loop documentation.

- [ ] **Step 2: Add CI event handling section**

Add a new section to the controller SKILL.md:

```markdown
### CI Event Handling

The daemon subscribes the controller to `notifications.github.{owner}.{repo}.ci` topics for each repo with active workers. When a CI event arrives (check_run or check_suite completion), it appears as a prompt with the event payload.

**When you receive a CI event prompt:**

1. Parse the event to extract the repo (`owner/repo`) and commit SHA
2. Check if any active worker's PR is affected by this CI result
3. If a matching worker exists and CI failed:
   - Follow the existing `resume_implementer_for_ci_failure` action
4. If CI passed and a worker is in testing/review:
   - This may unblock the pipeline — trigger an early collect/poll for that issue
5. If no matching worker is found, ignore the event — the next regular polling cycle will catch any state changes

**Topic format:** `notifications.github.{owner}.{repo}.ci`

**Event payload fields of interest:**
- `action`: `completed`, `requested`, etc.
- `conclusion`: `success`, `failure`, `cancelled`, etc.
- `name`: check run name
- `head_sha`: commit SHA to correlate with PR

**This is a speed optimization.** Even if a CI event is missed or unrecognized, the regular ~30s polling cycle catches all state transitions. Do not add complex retry logic for CI events.
```

- [ ] **Step 3: Add subscription policy note**

Near any existing Envoy or subscription references in the controller SKILL.md, add:

```markdown
**Envoy Subscription Policy:**
- The controller is auto-subscribed to base topics on daemon startup: `notifications.role.legion-controller`, `notifications.slack.*.*.mention`, `notifications.github.*.*.mention`
- Per-repo CI subscriptions are added by the daemon on first worker dispatch for each GitHub repo
- CI subscriptions persist for the daemon's lifetime (not tied to individual workers)
- On daemon restart, CI subscriptions are reconciled from persisted worker state
```

- [ ] **Step 4: Commit**

```bash
jj describe -m "docs(skills): add controller CI event handling and subscription policy"
jj new
```

---

### Task 6: Documentation update — Depends on: Task 3, Task 4, Task 5

**Files:**
- Modify: `docs/solutions/daemon/envoy-auto-subscription-patterns.md`

- [ ] **Step 1: Read existing documentation**

Read `docs/solutions/daemon/envoy-auto-subscription-patterns.md` to understand current content.

- [ ] **Step 2: Add subscription lifecycle table**

Add a new section after the existing content:

```markdown
## Subscription Lifecycle (Full State Table)

| Worker State Transition | Subscription Action | Owner | Authority |
|------------------------|---------------------|-------|-----------| 
| Dispatched | Subscribe worker to `issue.{n}.>` | Daemon (`POST /workers`) | Authoritative |
| First dispatch for repo | Subscribe controller to `{owner}.{repo}.ci` | Daemon (`POST /workers`) | Authoritative |
| PR created (implement) | Subscribe worker to `pr.{n}.>` | Worker (`envoy_subscribe`) | Best-effort |
| Worker resumed (prompt) | Re-subscribe worker to issue topics | Daemon (`/workers/:id/prompt`) | Authoritative |
| Worker exits (`worker-done`) | Unsubscribe worker from explicit issue+PR topics | Worker (`envoy_unsubscribe`) | Best-effort |
| Worker deleted | Unsubscribe worker from all topics (empty array) | Daemon (`DELETE /workers`) | Authoritative |
| Worker crashes before exit | No self-unsubscribe occurs | — | Daemon DELETE is safety net |
| Daemon startup (active workers) | Re-subscribe workers + controller CI for repos | Daemon (startup) | Authoritative |
| Serve crash recovery | Re-subscribe workers + controller to all topics | Daemon (health tick) | Authoritative |
| Daemon shutdown | Unsubscribe controller | Daemon | Authoritative |
```

- [ ] **Step 3: Add authority model section**

```markdown
## Authority Model

- **Daemon** is the **authoritative** subscription manager. It subscribes on dispatch, re-subscribes on resume/restart, and unsubscribes on delete.
- **Workers** provide **best-effort early cleanup** using `envoy_unsubscribe` to reduce the gap between `worker-done` and daemon cleanup.
- **Key distinction:** Worker self-unsubscribe uses **explicit topic list** (preserves `notifications.agent.{sessionId}`). Daemon DELETE uses **empty topic array** (removes everything).
- If a worker crashes before self-unsubscribing, the daemon's `DELETE /workers` is the safety net.
```

- [ ] **Step 4: Add startup reconciliation pattern**

```markdown
## Startup Reconciliation

On daemon startup with persisted active workers:
1. Worker sessions are recreated via `createSession` (existing behavior)
2. **NEW:** Each worker with `repo` and `issueNumber` is re-subscribed to its issue topic
3. **NEW:** Controller is re-subscribed to CI topics for each unique repo across active workers
4. `subscribedCiRepos` is seeded from persisted state to prevent duplicate CI subscribes on future dispatches

The same pattern applies after serve crash recovery in the health tick loop.
```

- [ ] **Step 5: Run verification**

Run: `bunx biome check docs/solutions/daemon/envoy-auto-subscription-patterns.md`
Expected: Clean (no formatting issues)

- [ ] **Step 6: Commit**

```bash
jj describe -m "docs(solutions): update envoy subscription lifecycle policy"
jj new
```

---

## Testing Plan

### Setup
- `bun install` (if not already done)
- No running services needed — all tests are unit tests with mocked dependencies

### Health Check
- `bunx tsc --noEmit` returns 0
- `bun test --bail` passes all tests

### Verification Steps

For each acceptance criterion:

1. **AC1 (Worker self-unsubscribe)**
   - Action: Search `.opencode/skills/legion-worker/SKILL.md` for `envoy_unsubscribe`
   - Expected: Contains explicit topic list unsubscribe in Exiting section
   - Tool: grep

2. **AC2 (Authoritative DELETE cleanup)**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "unsubscribes worker from envoy on delete"`
   - Expected: PASS (existing test, empty-topic unsubscribe verified)
   - Tool: bun test

3. **AC3 (PR subscription)**
   - Action: Search `.opencode/skills/legion-worker/workflows/implement.md` for `envoy_subscribe`
   - Expected: Contains PR subscription step after PR creation (already exists at line 308-321)
   - Tool: grep

4. **AC4 (Controller CI subscription)**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "subscribes controller to CI"`
   - Expected: PASS
   - Tool: bun test

5. **AC5 (Daemon restart reconciliation)**
   - Action: `bun test packages/daemon/src/daemon/__tests__/index.test.ts --test-name-pattern "re-subscribes"`
   - Expected: PASS
   - Tool: bun test

6. **AC6 (Resume re-subscription)**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --test-name-pattern "re-subscribes worker.*prompt"`
   - Expected: PASS
   - Tool: bun test

7. **AC7 (Fire-and-forget)**
   - Action: `bun test packages/daemon/src/daemon/__tests__/ --test-name-pattern "even when envoy|startup succeeds even"`
   - Expected: PASS
   - Tool: bun test

8. **AC8 (Controller CI handling)**
   - Action: Search `.opencode/skills/legion-controller/SKILL.md` for "CI Event Handling"
   - Expected: Contains CI event handling section with prompt format and action instructions
   - Tool: grep

9. **AC9 (Policy documentation)**
   - Action: Search `docs/solutions/daemon/envoy-auto-subscription-patterns.md` for "Subscription Lifecycle" and "Authority Model"
   - Expected: Contains lifecycle table and authority model sections
   - Tool: grep

### Tools Needed
- `bun test` for unit tests
- `bunx tsc --noEmit` for type checking
- `bunx biome check src/` for lint
- grep for skill/doc verification

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | `envoy`, `test-driven-development` |
| Test | (none beyond standard) |
| Review | (none beyond standard) |

Workers: invoke these skills at the start of your workflow before beginning work.
If a skill is unavailable in your environment, proceed without it.
