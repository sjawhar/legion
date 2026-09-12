# `reconnectWorkers` after `api` — implementation plan (LEGION-11)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (one
> implementer, one PR against `sjawhar/legion`, branch `legion/LEGION-11`). Steps use `- [ ]` for
> tracking. Every ruling from a review round supersedes this document.

**Goal:** A daemon restart fully retires a worker that died while the daemon was down — capability
revoked, pane killed, locator cleared, secret file removed — with no `worker reconnection failed`
line and no `TypeError` in the boot log.

**Architecture:** `startDaemonLocked` (`packages/daemon/src/daemon/index.ts`) builds a
`ProcessManager` whose four capability deps (`mintControllerCapability`, `mintBootToken`,
`mintWorkerBootToken`, `revokeSessionCapability`) read a `let api: LegionApi` by reference. Today
`reconnectWorkers()` and `pruneSecretFiles()` run before `api = startLegionApi(...)`, so a dead
ready-confirmed worker's retirement (`markWorkerDead` → `markWorkerDeadLocked` →
`retireWorkerLocator` → `revokeRoleClaim` → `deps.revokeSessionCapability`) dereferences
`undefined`, the `Promise.all` in `reconnectWorkers` rejects, `index.ts` logs
`[legion] worker reconnection failed: TypeError: undefined is not an object (evaluating
'api.revokeSessionCapability')`, and the claim keeps its locator (a held running-worker slot), its
still-valid capability, and its `<state_dir>/secrets/<token>` file. The fix moves three boot steps —
`reconnectWorkers` (try/`console.error` verbatim), `pruneSecretFiles()`, and the pending-controller-
notice drain/spawn block — to immediately after `api = startLegionApi(...)` and before
`enableWorkerPromotion()`. Nothing else changes behaviour.

**Tech stack:** TypeScript on Bun; Bun test (`bun:test` `spyOn`); tmux 3.7c private server
`tmux -L legion-<project>`; jj.

**Spec:** `dispatch://LEGION-11/spec` (version 3; design gate approved by Sami 2026-09-12 via
`dispatch://LEGION-11/ask/522eaa0a-da53-44bc-96f6-3784203294bb`). This plan argues from it:
Acceptance 1–4, Design 1–5, Errors, Testing, Rejected.

## Decisions needed

None. One scope point was raised to the architect and ruled during planning (verbatim ruling,
2026-09-12T04:22Z, envoy id `4557e89b0fb6cfb523fd42643c7d047c`): *"Trim the false clause in
processes.ts retireUnconfirmedBoot's doc comment (lines ~912-916), and update the two
worker-admission.ts comments (120-129, 327-330) and the comment-only lines in processes.test.ts
(349, 7304-7305, 7340-7343, 8866-8867). Design 5 meant no behaviour change in processes.ts, api.ts,
or api/auth.ts; comments describing the old order must not survive the move. I am amending the
spec's Design 5 to say exactly that so the reviewer holds the same contract."* Task 3 carries it.

## Global constraints

- Spec Design 5 (as amended above): **no behaviour change** in `processes.ts`, `api.ts`,
  `api/auth.ts`. Comment-only edits in `processes.ts` are in scope; nothing executable moves there.
- Spec Requirements: no `if (api)` guard, no deferred/replayed revoke, no split of
  `startLegionApi` into create/listen (all three are in the spec's Rejected list). A future
  reintroduction of the ordering bug must still crash loud.
- `reconnectWorkers` itself is untouched: same probe, same verdicts, same logging.
- The production daemon (`legion-sjawharlegion`, port 13370, pid of
  `bun run packages/daemon/src/cli/index.ts start sjawhar/legion`) is **never** restarted, stopped,
  or pointed at this branch. The tester's rig is a separate project/state_dir/tmux server/port.
- Repo rules: `node:` imports, Biome (double quotes, semicolons, 100 cols), `type` imports, no
  barrel files, co-located `__tests__/`, comments describe current behaviour, jj not git. Gates run
  once at the end of Task 4, never mid-task; never `| head` / `| tail` on gate commands.
- Every commit: `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "<message>"`
  (or `jj split` for a subset of files). Never `jj abandon`, never `jj edit @-`. The working copy
  carries an untracked-on-main, empty `.omp/config.yml` the OMP session provisioned — leave it in
  the working-copy commit, never commit it (use `jj split <paths>` or a fileset that excludes it),
  and point the bookmark at the last described commit before pushing.
- `bun install` has not been run in this workspace (no `node_modules`); Task 1 Step 0 does it.

## Where the bug lives (read before Task 1)

`packages/daemon/src/daemon/index.ts` on the branch parent `7db2cd83` (main `94ad34dd1` as cited
by the architect has the identical region):

| lines | today |
| :--- | :--- |
| 373 | `let api: LegionApi;` — `undefined` until 542. |
| 385–389 | the four closures: `async () => api.mintControllerCapability()`, `(tree, generation) => api.mintBootToken(...)`, `(...) => api.mintWorkerBootToken(...)`, `(sessionId) => api.revokeSessionCapability(sessionId)`. |
| 401–408 | the comment claiming `reconnectWorkers` "needs no `api` reference". False: `processes.ts:2848-2853` `retireWorkerLocator` calls `revokeRoleClaim` (2691) which calls `deps.revokeSessionCapability` whenever the claim has a `sessionId`. |
| 409–413 | `try { await processManager.reconnectWorkers(); } catch (error) { console.error(\`[legion] worker reconnection failed:\`, error); }` |
| 415 | `await processManager.pruneSecretFiles();` |
| 457–479 | the pending-controller-notice comment + `if (state.controllerPendingNotices.length > 0) { drain \| ensureController }` — `ensureController` (`processes.ts:1409`) calls `deps.mintControllerCapability()`; today it survives only because `ensureController` awaits `controllerAlive()` (a tmux call) before minting. |
| 542–550 | `api = startLegionApi({...}, apiDeps);` |
| 552–563 | comment + `processManager.enableWorkerPromotion();` |
| 568–570 | `reconnectRoots()`, `await reconcileAdmission()`, `await reconcileWorkerAdmission()`. |

Real-world signature (captured by the LEGION-6 tester on 2026-09-11, `/tmp/legion6-smoke/rig/daemon.log.round2b-typeerror`):

```
[legion] failed to reconnect worker legion-sjawhar24-legsmoke-25-planner: ... error: Failed to connect
[legion] worker reconnection failed: ...
409 |     revokeSessionCapability: (sessionId) => api.revokeSessionCapability(sessionId),
                                                  ^
TypeError: undefined is not an object (evaluating 'api.revokeSessionCapability')
      at revokeSessionCapability (.../packages/daemon/src/daemon/index.ts:409:45)
      at revokeRoleClaim (.../packages/daemon/src/daemon/processes.ts:2688:37)
      at retireWorkerLocator (.../packages/daemon/src/daemon/processes.ts:2847:10)
      at markWorkerDeadLocked (.../packages/daemon/src/daemon/processes.ts:763:16)
legion daemon listening on 127.0.0.1:19370
```

The same fault reaches the *unconfirmed*-boot path too (`retireUnconfirmedBoot` → `retireWorkerLocator`,
`processes.ts:963`) whenever that claim already has a `sessionId` (registered via `/worker/started`,
never reached `/worker/ready`). The move fixes both; the regression test pins the ready-confirmed
path the spec names.

## Closure audit (spec Requirement 2; goes into the PR body)

Every path that can execute one of the four `api`-reading deps, and why none can run before
`api = startLegionApi(...)` once the three blocks move:

| dep | direct caller | every path into it | reachable before `api` after the move? |
| :--- | :--- | :--- | :--- |
| `mintControllerCapability` | `ensureController` (`processes.ts:1409`) | `handleException` (1799; NATS exception lane via `eventDeps.onException`), `onUndeliverable` (`index.ts:427`; event-pump 404 recovery), `retireAndRespawnStuckController` (1680; timer armed only inside `ensureController`), **boot pending-notices block (moved)** | No — NATS callbacks and timers only fire on later event-loop turns; the block is moved. |
| `mintBootToken` | `spawnRoot` (2050) | `startRoot` (1865) ← `admit` (426; `eventDeps.onAdmit`, NATS) and the `reconcileAdmission`/`releaseSlot` promotion sweeps (`index.ts:569`, and closeTree/beginLinger/launch-failed paths via API/NATS/timers); `resurrectDeadTree` (2905) ← `resurrect` ← `onProbe` (NATS/resync), `handleException` (1814), `escalateOrRetryUnconfirmedRoot` retries (1619/1641; root deadline timers armed by `reconnectRoots` at `index.ts:568` or `spawnTree`) | No — `reconcileAdmission`/`reconnectRoots` run after `enableWorkerPromotion()`; everything else is NATS/HTTP/timer-driven. |
| `mintWorkerBootToken` | `launchWorker` (2459) | `WorkerAdmissionDeps.launchWorker` (328) ← `launchOrQueue` (`spawnWorker`, HTTP `/worker/spawn`) and `promoteQueuedWorker`/`drainWorkerQueue` (gated by `workerPromotionEnabled`, flipped at `index.ts:563`) | No — HTTP handlers exist only after `api`; promotion is gated until `enableWorkerPromotion()`. |
| `revokeSessionCapability` | `revokeRoleClaim` (2691) | `retireWorkerLocator` (2851) ← `markWorkerDeadLocked` (763) ← `markWorkerDead` (773) ← **`reconnectWorkers` (818, moved)**, `onWorkerClientClosed` (2321; a socket-close callback — the first connection is opened by `reconnectWorkers`), `retireDeadClaim` (332; promotion path); ← `retireUnconfirmedBoot` (963) ← **`reconnectWorkers` (820, moved)**, boot watchdog dead verdict (armed in `reconnectWorkers`/`launchWorker`), `onWorkerClientClosed`; ← `spawnWorker` (582, HTTP); ← `launchWorker` (2541). Also `closeTreeLocked` (1094/1158/1202; HTTP `/process/exit`, linger timer, NATS linger), `escalateOrRetryUnconfirmedRoot` (1558; root deadline timer), `removeTreeWindow` (2704; resurrect/closeTree) | No — the only boot-time caller is `reconnectWorkers`, now after `api`; every other path needs a connection, HTTP request, NATS message, or timer that cannot exist before `api` is assigned. |

Structural argument (the invariant the new `index.ts` comment states): after the move, the span
from `new ProcessManager({...})` to `api = startLegionApi(...)` contains **no `await`** — only
closure/object construction, the synchronous `startEventPump(eventDeps)` (registers NATS callbacks;
delivery happens on later turns), and the synchronous `createCiStatusFetcher(...)`. JavaScript
cannot run a callback inside a synchronous span, so no `processManager` method executes before
`api` is assigned. Verified by reading `index.ts:375-550` on the branch parent: the only awaits in
373–570 today are `reconnectWorkers` (410), `pruneSecretFiles` (415), and the three at 569–570
that already run after `api`.

## File structure

| file | change |
| :--- | :--- |
| `packages/daemon/src/daemon/index.ts` | Move lines 401–415 and 457–479 to after `api = startLegionApi(...)`; replace the 401–408 comment with the ordering invariant. Only behaviour change in the PR. |
| `packages/daemon/src/daemon/__tests__/index.test.ts` | New regression test in `describe("startDaemon")`; imports `spyOn`, `roleToken`, `writeSecretFile`, `LegionState`, `DaemonStateResponse`. |
| `packages/daemon/src/daemon/worker-admission.ts` | Two doc comments (120–129, 327–330) rewritten to the new rationale. Comment-only. |
| `packages/daemon/src/daemon/processes.ts` | One clause trimmed from `retireUnconfirmedBoot`'s doc comment (912–916). Comment-only. |
| `packages/daemon/src/daemon/__tests__/processes.test.ts` | Comment-only lines 348–350, 7302–7305, 7340–7343, 8866–8867. No assertion changes. |
| `packages/daemon/src/daemon/AGENTS.md` | One new "Operational invariants" bullet: the boot ordering. |
| `docs/solutions/daemon/refcounted-hold-guards-a-derived-prune.md` | The "Needs its own issue" open edge (95–97) now points at LEGION-11 as fixed. |

Task order: 1 → 2 → 3 → 4. Tasks 1 and 2 are the TDD pair (test red on the branch parent, green
after the move). Task 3 is the comment/doc sweep. Task 4 is gates, commits, push, PR.

---

### Task 1: the regression test (red)

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/index.test.ts` (imports at 1–15; new `it` inside
  `describe("startDaemon")`, after the test ending at line 554 "arms a restored root's registration
  deadline …" so it sits with the other restart-shaped tests).

**Interfaces — consumes:** the file's existing `config(stateDir)`, `daemonTestDependencies(nats,
publications, onControllerSecret)`, `FakeNats`, `newLegionState`; `writeSecretFile` from
`../secrets`; `roleToken` from `@legion/contracts`.

- [ ] **Step 0: install workspace dependencies once**

```bash
cd -- "$LEGION_WORKSPACE" && bun install --frozen-lockfile
```

Expected: exits 0; `node_modules/` appears (gitignored).

- [ ] **Step 1: add the imports**

Line 1 becomes `import { describe, expect, it, spyOn } from "bun:test";`. Line 6 becomes
`import { controllerToken, type DaemonStateResponse, roleToken, roleTopic } from "@legion/contracts";`.
Line 12 becomes `import { type LegionState, newLegionState } from "../legion-state";`. Add, after
the `../nats-transport` import: `import { writeSecretFile } from "../secrets";`. Biome sorts
imports; run it in Task 4 only.

- [ ] **Step 2: write the failing test**

Insert after the `it("arms a restored root's registration deadline …")` block (ends line 554):

```ts
  it("retires a ready-confirmed worker whose shim socket is dead at boot: capability revoked through the live api, locator cleared, secret file removed", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const issue = "WIDGETS-42";
    const token = roleToken(daemonConfig.project, issue, "tester");
    const ompSessionFile = path.join(stateDir, "workers", "dead-tester.session.json");
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    // A confirmed, previously-live worker: `sessionId` gives its retirement a capability to
    // revoke (the `api` dereference), and `readyConfirmedAt` routes `reconnectWorkers` through
    // `markWorkerDead`, not the unconfirmed-boot retirement.
    state.roles[token] = {
      issue,
      role: "tester",
      generation: 1,
      sessionId: "ses_dead_tester",
      readyConfirmedAt: Date.parse("2026-08-23T00:00:00.000Z"),
      locator: {
        tmuxSession: `legion-${daemonConfig.project}`,
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: path.join(stateDir, "workers", "dead-tester.sock"),
        ompSessionFile,
      },
    };
    // The pane's boot-token file a previous daemon process wrote; boot-time hygiene must reap it
    // once the locator clears.
    const secretFile = await writeSecretFile(stateDir, token, "stale-boot-token");
    const saved: LegionState[] = [];
    const killedPanes: string[] = [];
    const errorLogs: unknown[][] = [];
    const consoleError = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      errorLogs.push(args);
    });
    const options = daemonTestDependencies(new FakeNats(), [], () => {});
    const baseRunner = options.deps?.runner;
    if (!baseRunner) throw new Error("daemonTestDependencies did not supply a runner");
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          ...options.deps,
          loadState: async () => state,
          saveState: async (_file, snapshot) => {
            saved.push(structuredClone(snapshot));
          },
          connectWorkerRpc: async () => {
            throw new Error("ECONNREFUSED: worker shim socket unreachable");
          },
          runner: async (command, runnerOptions) => {
            if (command[0]?.endsWith("/tmux") && command[3] === "kill-pane") {
              killedPanes.push(command[5] ?? "");
              return { stdout: "", stderr: "can't find pane: %7", exitCode: 1 };
            }
            return baseRunner(command, runnerOptions);
          },
        },
      });

      // The whole retirement ran: no reconnect-wide failure and no TypeError anywhere in the boot
      // log. (`[legion] failed to reconnect worker …: ECONNREFUSED` is the expected per-worker
      // verdict line and is not a failure.)
      const failures = errorLogs.filter((args) =>
        args.some(
          (arg) =>
            arg instanceof TypeError ||
            (typeof arg === "string" && arg.includes("worker reconnection failed"))
        )
      );
      expect(failures).toEqual([]);
      expect(killedPanes).toEqual(["%7"]);

      const persisted = saved.at(-1)?.roles[token];
      if (!persisted || !("issue" in persisted)) throw new Error("worker claim was not persisted");
      expect(persisted.locator).toBeUndefined();
      expect(persisted.resumeSessionFile).toBe(ompSessionFile);
      expect(persisted.sessionId).toBe("ses_dead_tester");

      const response = await fetch(`http://127.0.0.1:${daemon.server.port}/legion/v1/state`);
      expect(response.status).toBe(200);
      const body = (await response.json()) as DaemonStateResponse;
      const exposed = body.roles[token];
      if (!exposed) throw new Error("worker role missing from GET /legion/v1/state");
      expect(exposed.locator).toBeUndefined();
      expect(exposed.sessionId).toBe("ses_dead_tester");

      await expect(stat(secretFile)).rejects.toThrow(/ENOENT/);
    } finally {
      consoleError.mockRestore();
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
```

Why each assertion is a consumer-observable contract, not plumbing: the log lines are what an
operator greps (`daemon.log`); `killedPanes` is the tmux side effect the retirement must reach; the
`saveState` snapshot is the durable state a restart reloads; `GET /legion/v1/state` is what
`legion state` prints; the secret file is the credential hygiene guarantee (`AGENTS.md`: "a pane
file lives exactly as long as its locator"). `resumeSessionFile` is the spec's Errors row ("its
saved-session file reference is kept so it can be resumed later").

- [ ] **Step 3: run it — expect RED for the right reason**

```bash
cd -- "$LEGION_WORKSPACE" && bun test packages/daemon/src/daemon/__tests__/index.test.ts -t "retires a ready-confirmed worker"
```

Expected: 1 fail at `expect(failures).toEqual([])` — the received array holds one entry
`["[legion] worker reconnection failed:", TypeError: undefined is not an object (evaluating
'api.revokeSessionCapability')]`. Reason on the unfixed tree: `reconnectWorkers` runs at
`index.ts:410` while `api` is still `undefined`; `markWorkerDeadLocked` throws at
`retireWorkerLocator` → `revokeRoleClaim` before `stopProcess` (so `killedPanes` is `[]`), before
`delete current.locator` (so every later `saveState` snapshot and `/state` still show the locator),
and `pruneSecretFiles()` then keeps the file because `liveSecretFiles()` still sees that locator.
The `processes.test.ts` reconnect cases cannot catch this: their `manager()` helper injects
`revokeSessionCapability: (sessionId) => revokedSessions.push(sessionId)` (`processes.test.ts:321`)
— only `index.test.ts` exercises the real closure.

If instead the test errors on a type/import problem, fix that first; the red must be the
`TypeError` assertion.

- [ ] **Step 4: do not commit yet** — the test and the fix land in one commit (Task 2 Step 5).

---

### Task 2: move the three boot steps after `api` (green)

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts:401-415, 457-479, 542-563`

**Interfaces — produces:** nothing new. `startDaemon`'s resolved-after-boot-settles contract is
unchanged: `reconnectWorkers` and `pruneSecretFiles` are still awaited before `startDaemon`
resolves; the two `void` branches of the notice block remain fire-and-forget.

- [ ] **Step 1: delete lines 401–415** (the "needs no `api` reference" comment, the
  `reconnectWorkers` try/catch, the prune comment and call). Nothing replaces them at this spot.

- [ ] **Step 2: delete lines 457–479** (the pending-controller-notice comment and `if` block).
  `const eventPump: EventPump = startEventPump(eventDeps);` (456) is now directly followed by
  `const fetchCiStatusBatch = createCiStatusFetcher(...)` (480).

- [ ] **Step 3: insert the moved blocks after `api = startLegionApi(...)`** — i.e. after the
  `);` that closes the call (old line 550) and before the comment that begins "Awaited only now
  that `api` is assigned" (old 552). Insert exactly this (the try/catch, the prune line, and the
  notice block are byte-for-byte the removed text; only the leading comment is new and one
  sentence is appended to the notice comment):

```ts
  // Nothing on `processManager` runs before this point. Its `mintControllerCapability`,
  // `mintBootToken`, `mintWorkerBootToken`, and `revokeSessionCapability` deps read `api` by
  // reference, and every path into them — a dead worker's retirement (`retireWorkerLocator` ->
  // `revokeRoleClaim` -> `deps.revokeSessionCapability`), `ensureController`, root and worker
  // launches — is reachable only from here on. The span from `new ProcessManager(...)` to the
  // assignment above contains no `await`, so no callback can interleave with it.
  //
  // `reconnectWorkers` therefore runs here: after `api` because retiring a confirmed-dead worker
  // revokes its capability through it, and awaited before `enableWorkerPromotion()` below because
  // it is the source of truth `runningWorkerCount()` relies on — an admission decision racing
  // ahead of it would decide against a count that still holds every unprobed claim as running.
  // A reconnect's own `get_state` response can still synchronously fire `onIdle` ->
  // `promoteWorkerQueue`; that trigger (and `reconcileWorkerAdmission`) stay gated behind
  // `enableWorkerPromotion()` until the probe has settled. `Bun.serve` is already accepting
  // requests while this runs: safe, because every retirement re-validates the claim it was
  // handed under `mutateClaim(token)`, an unprobed claim counts as running, and promotion stays
  // gated.
  try {
    await processManager.reconnectWorkers();
  } catch (error) {
    console.error(`[legion] worker reconnection failed:`, error);
  }
  // Reaps pane secret files a crash left behind between clearing a locator and its save's prune.
  await processManager.pruneSecretFiles();
  // A crash between `/controller/ready` persisting its own role claim (`ctx.save()`) and that
  // same request finishing its own drain (`onControllerReady`, below) would otherwise strand
  // every notice already recorded in `controllerPendingNotices` forever: the controller session
  // that already claimed the role will never POST `/controller/ready` again this boot, so
  // nothing else would ever trigger a drain for it. Keyed off the durable queue itself, not
  // just a live claim: if no controller role exists at all (its own process died too, or one
  // never existed for this project), nothing would ever reach `/controller/ready` to trigger a
  // drain on its own -- `ensureController` spawns one directly, and its own eventual
  // `/controller/ready` call drains these same notices through the ordinary path once it's
  // live. Both branches are fire-and-forget: `drainControllerNotices` already retries a failed
  // publish with its own bounded backoff, `ensureController` is idempotent, and nothing else in
  // boot depends on either finishing. Runs after `api` is assigned because `ensureController`
  // mints the controller capability through it.
  if (state.controllerPendingNotices.length > 0) {
    if (state.roles[controllerToken(state.project)]) {
      void eventPump.drainControllerNotices().catch((error) => {
        console.error(`[legion] boot-time controller notice drain failed:`, error);
      });
    } else {
      void processManager.ensureController().catch((error) => {
        console.error(`[legion] boot-time controller spawn for pending notices failed:`, error);
      });
    }
  }
```

The existing comment that follows ("Awaited only now that `api` is assigned: the promotion
cascade …") and `processManager.enableWorkerPromotion();` stay verbatim — every sentence in it is
still true. The `onControllerReady` comment inside `apiDeps` (old 524–531) references
`drainControllerNotices` "below"; that word referred to `onControllerReady` itself and stays.

Resulting order in `startDaemonLocked`: `new ProcessManager` → closures/`eventDeps` →
`startEventPump` → `fetchCiStatusBatch`/`emitResync`/`apiDeps` → `api = startLegionApi(...)` →
`reconnectWorkers` (awaited) → `pruneSecretFiles` (awaited) → pending-notice drain/spawn (void) →
`enableWorkerPromotion()` → `reconnectRoots()` → `reconcileAdmission` → `reconcileWorkerAdmission`
→ timers → return.

- [ ] **Step 4: run the regression test — expect GREEN**

```bash
cd -- "$LEGION_WORKSPACE" && bun test packages/daemon/src/daemon/__tests__/index.test.ts
```

Expected: every test in the file passes, including the new one. If
`spawns the controller at boot when notices are pending but no controller role claim exists` or
`drains a pending controller notice at boot …` fail, the notice block was not moved intact (both
rely on it still running during boot; it now runs after the probe, before `startDaemon` resolves
its fire-and-forget branch, and those tests already `flushEventLoopUntil` their observable).

- [ ] **Step 5: commit test + fix as one change**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" split -m "fix(daemon): run reconnectWorkers after the API exists so a dead worker's retirement can revoke its capability

startDaemonLocked ran reconnectWorkers (and pruneSecretFiles, and the pending-controller-notice
drain/spawn) before \`api = startLegionApi(...)\`, but ProcessManager's revokeSessionCapability dep
reads \`api\` by reference: a ready-confirmed worker found dead at boot threw
\`TypeError: undefined is not an object (evaluating 'api.revokeSessionCapability')\` inside
markWorkerDeadLocked, the whole reconnect rejected (\`worker reconnection failed\`), and the claim
kept its locator, its running-worker slot, its valid capability, and its secrets/<token> file.
The three blocks now run immediately after the API is assigned and before enableWorkerPromotion(),
so the probed running-worker count is still what promotion trusts. reconnectWorkers itself is
unchanged. Regression test boots the daemon through the real closures with a dead-socket,
ready-confirmed claim and a pre-seeded secret file.

Dispatch: LEGION-11" packages/daemon/src/daemon/index.ts packages/daemon/src/daemon/__tests__/index.test.ts
```

(`jj split <paths>` moves exactly those two files into a new described commit and leaves the
working copy — with the untracked `.omp/config.yml` — on top.)

---

### Task 3: comments and docs that described the old order

**Files:**
- Modify: `packages/daemon/src/daemon/worker-admission.ts:120-129, 327-330`
- Modify: `packages/daemon/src/daemon/processes.ts:912-916` (comment only)
- Modify: `packages/daemon/src/daemon/__tests__/processes.test.ts:348-350, 7302-7305, 7340-7343, 8866-8867` (comments only)
- Modify: `packages/daemon/src/daemon/AGENTS.md` ("Operational invariants" list)
- Modify: `docs/solutions/daemon/refcounted-hold-guards-a-derived-prune.md:95-97`

Rationale for the gate's new wording: with `api` present during `reconnectWorkers`, the
`workerPromotionEnabled` gate no longer prevents a missing-`api` mint; it still prevents a drain
mid-probe. `runningWorkerCount()` (`worker-admission.ts:205-217`) counts every claim with a locator
and no cached client as running, so a drain fired by one worker's `onIdle` while its siblings are
still unprobed would decide against a count that has not yet become the probed truth, and would
launch fresh panes interleaved with the very probe that may still retire or resume their
siblings. The gate keeps the first drain the boot sequence's own ordered
`reconcileWorkerAdmission()`. No assertion in any test changes.

- [ ] **Step 1: `worker-admission.ts:120-129`** — replace the `workerPromotionEnabled` doc comment
  with:

```ts
  /** Gates every worker-queue drain (`promoteWorkerQueue`'s fire-and-forget trigger and
   * `reconcileWorkerAdmission`'s explicit call alike — neither bypasses this) until
   * `enableWorkerPromotion` flips it. False from construction protects boot: `reconnectWorkers`
   * runs first (after the daemon's HTTP `api` is assigned, since a dead worker's retirement
   * revokes its capability through it), and a reconnect's own `get_state` -> `isStreaming: false`
   * -> `onIdle` can synchronously trigger `promoteWorkerQueue` while other claims are still
   * unprobed — every one of which `runningWorkerCount()` still counts as running. A drain at that
   * point would decide against a count that is not yet the probed truth and launch fresh panes
   * interleaved with the probe that may still retire (or resume) their siblings; the gate keeps
   * the first drain the boot sequence's own ordered `reconcileWorkerAdmission()` call, after every
   * probe has settled.
   */
```

- [ ] **Step 2: `worker-admission.ts:327-330`** — in `enqueueForRetry`'s doc comment, replace
  `a restart-time caller (\`reconnectWorkers\`, run before \`api\` exists) leaves the token for the`
  with `a restart-time caller (\`reconnectWorkers\`, run before \`enableWorkerPromotion()\`) leaves the token for the`.

- [ ] **Step 3: `processes.ts:912-916`** — in `retireUnconfirmedBoot`'s doc comment, the sentence
  currently reading

  > a direct `launchWorker` call would bypass the running-worker cap, the reservation, and the per-role launch lock — over-admission, or a second pane racing a concurrent same-role spawn — and would also dereference `api` before it exists when this runs from `reconnectWorkers` at boot (before `enableWorkerPromotion()`).

  becomes

  > a direct `launchWorker` call would bypass the running-worker cap, the reservation, and the per-role launch lock — over-admission, or a second pane racing a concurrent same-role spawn.

  Nothing else in `processes.ts` changes. Reflow the comment to 100 columns.

- [ ] **Step 4: `processes.test.ts` comment lines**
  - 348–350: `(index.ts calls this immediately after \`api\` is assigned)` →
    `(index.ts calls this once \`reconnectWorkers\` has settled, after \`api\` is assigned)`.
  - 7302–7305: replace `while still inside reconnectWorkers, before \`api\` (and this gate) would ever be assigned in the real boot sequence.` with
    `while still inside reconnectWorkers, before enableWorkerPromotion() is called in the real boot sequence.`
  - 7340–7343: replace `no boot token was ever minted (the real bug this fixes — \`mintWorkerBootToken\` reads \`api\` by reference, which does not exist yet at this exact point in the real boot sequence), and the queued assignment is untouched.` with
    `no boot token was ever minted (a launch mid-probe would decide against a running-worker count that still holds every unprobed claim as running), and the queued assignment is untouched.`
  - 8866–8867: replace `the daemon calls it before \`api\` exists, hence before \`enableWorkerPromotion()\`.` with
    `the daemon calls it before \`enableWorkerPromotion()\`.`

- [ ] **Step 5: `packages/daemon/src/daemon/AGENTS.md`** — append one bullet to
  "## Operational invariants":

```md
- Boot ordering in `startDaemonLocked` (`index.ts`): no `ProcessManager` method runs before
  `api = startLegionApi(...)`. The manager's `mintControllerCapability`/`mintBootToken`/
  `mintWorkerBootToken`/`revokeSessionCapability` deps read `api` by reference, and every path into
  them — a dead worker's retirement (`retireWorkerLocator` → `revokeRoleClaim`), `ensureController`,
  root and worker launches — is reachable only afterwards. `reconnectWorkers` therefore runs after
  `api` is assigned (its retirements revoke through it) and, awaited, before
  `enableWorkerPromotion()` (promotion trusts the probed running-worker count); `pruneSecretFiles()`
  and the pending-controller-notice drain/spawn follow it in that same window. The API is already
  accepting requests while the probe runs; that is safe because every retirement re-validates its
  claim under `mutateClaim(token)`, an unprobed claim counts as running, and promotion stays gated.
```

- [ ] **Step 6: `docs/solutions/daemon/refcounted-hold-guards-a-derived-prune.md:95-97`** —
  replace the bullet with:

```md
- Was pre-existing on `main`: `index.ts` ran `reconnectWorkers` before `api` was assigned, so
  `markWorkerDead → revokeRoleClaim → api.revokeSessionCapability` threw for a confirmed-dead
  worker at boot. Fixed by LEGION-11 (`reconnectWorkers` now runs after `api`, before
  `enableWorkerPromotion()`).
```

- [ ] **Step 7: commit the sweep**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" split -m "docs(daemon): comments and invariants follow reconnectWorkers to its post-API position

The workerPromotionEnabled gate's reason is now the unprobed running-worker count, not a missing
\`api\`; retireUnconfirmedBoot's doc comment drops its \`api\`-before-exists clause; test comments and
the daemon AGENTS.md state the boot ordering invariant; the LEGION-6 retro's open edge points at
LEGION-11.

Dispatch: LEGION-11" packages/daemon/src/daemon/worker-admission.ts packages/daemon/src/daemon/processes.ts packages/daemon/src/daemon/__tests__/processes.test.ts packages/daemon/src/daemon/AGENTS.md docs/solutions/daemon/refcounted-hold-guards-a-derived-prune.md
```

---

### Task 4: gates, push, pull request

- [ ] **Step 1: the three gates, once, from the workspace root**

```bash
cd -- "$LEGION_WORKSPACE" && bunx biome check packages/daemon/src
cd -- "$LEGION_WORKSPACE/packages/daemon" && bunx tsc --noEmit   # tsconfig.json lives in the package, not the root
cd -- "$LEGION_WORKSPACE" && bun test packages/daemon
```

Expected: biome reports nothing (if it reformats the new comment/test, apply with
`bunx biome check --write packages/daemon/src` and amend the relevant commit with
`jj -R "$LEGION_WORKSPACE" squash --into <commit> <paths>`); tsc reports nothing; every daemon test
passes — in particular the three `processes.test.ts` reconnect cases (`mints nothing for a queued
worker when an idle reconnect fires onIdle before enableWorkerPromotion …` at 7293, `clears a
claim's stale locator when reconnectWorkers finds its socket dead …` at 8604, `clears a dead
worker's locator on boot when the private tmux socket does not exist yet …` at 8632) pass
unmodified: none of them asserts anything about `api`; they drive `ProcessManager` directly with an
injected `revokeSessionCapability`.

- [ ] **Step 2: ancestry check, bookmark, push**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 6)'
```

Expected chain: working copy (`.omp/config.yml` only, no description) → docs sweep → fix+test →
`plan: record handoff` → plan doc → `7db2cd83 chore: release cli v1.1.2`. Then:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" bookmark set legion/LEGION-11 -r @- && jj -R "$LEGION_WORKSPACE" git push --bookmark legion/LEGION-11
```

(`-r @-` keeps the undescribed working-copy commit off the branch.)

- [ ] **Step 3: open the PR** with `legion gh -- pr create --repo sjawhar/legion --base main --head legion/LEGION-11 --title "fix(daemon): run reconnectWorkers after the API exists so a dead worker's retirement can revoke its capability" --body-file <file>`. The body, in the merge queue's READY format from the skill, must contain:

```
Dispatch: LEGION-11

## What moved (spec Acceptance 4)
`startDaemonLocked` now runs, in this order, immediately after `api = startLegionApi(...)` and before `processManager.enableWorkerPromotion()`: (1) `await processManager.reconnectWorkers()` (try/`console.error` unchanged), (2) `await processManager.pruneSecretFiles()`, (3) the `controllerPendingNotices` drain / `ensureController` block. Previously all three ran before `api` was assigned. `reconnectWorkers`, `processes.ts`, `api.ts`, `api/auth.ts` behaviour: unchanged.

## Closure audit (spec Requirement 2)
<paste the "Closure audit" table and the structural argument from docs/plans/2026-09-12-reconnect-workers-after-api.md>

## Verification
**CI:** `pr-checks-result` run <run-id> — success at <head-sha>.
**Threads:** 0 resolved, 0 unresolved.
**Thermo:** (reviewer fills at review head)
**E2E:** (tester fills — see plan §Tester)
**Fast-follow:** none.
**Chain:** not stacked.
```

- [ ] **Step 4: watch CI** with `legion gh -- pr checks <n> --watch --repo sjawhar/legion`; fix
  failures on the branch (new commits, never rewrite pushed ones once someone stacks on them).

- [ ] **Step 5: write `.legion/implement.json`** (fields: `commits` with ids, `pr` url and number,
  `deviationsFromPlan` — expected `[]`, `testerNotes` pointing at §Tester below and at the exact
  main-side comparison source in it), commit it with
  `jj -R "$LEGION_WORKSPACE" split -m "implement: record handoff" .legion/implement.json`, re-point
  the bookmark at `@-`, push, then `legion handoff complete --summary '…'`.

---

## Tester (spec Acceptance 3; fills the PR body's **E2E** line)

Read `skill://using-secrets` before the first `secrets` call. All artifacts under
`/tmp/legion11-smoke`; nothing touches the production daemon (`legion-sjawharlegion`, port 13370,
state at the path in `/home/ubuntu/.config/legion/sjawhar-legion/legion.yaml`).

**Two runs, main first.** "Main" source: `/home/ubuntu/legion-ws-RunDaemon` — the checkout the
production daemon runs from (its `@` is `daemon run checkout: main` with an empty diff over its
main-lineage parent; record `jj -R /home/ubuntu/legion-ws-RunDaemon log -r @- --no-graph -T
'commit_id.short() ++ " " ++ description.first_line()'` in the evidence). Use it read-only: only
`bun run <path>`, never `jj` mutations there. "Branch" source: `$LEGION_WORKSPACE` at the PR head
(record `jj log -r @-`).

Prerequisites, once:

```bash
SMOKE=/tmp/legion11-smoke; mkdir -p "$SMOKE"
# Isolated NATS with JetStream. No stream is created: the daemon's two durable consumers log
# "durable consumer … stopped unexpectedly; restarting" every few seconds — expected noise here.
docker run -d --name legion11-nats -p 127.0.0.1:14223:4222 nats:2.10 -js
# DISPATCH_URL is not a secret: read it from this box's envoy.json (.dispatch.serverUrl).
```

Per run `R` ∈ {`main`, `branch`} with `SRC` = the source root above, `STATE=$SMOKE/$R/daemon`:

```bash
mkdir -p "$STATE"
cat >"$SMOKE/$R.yaml" <<EOF
project: sjawhar/11
port: 19371
nats_urls:
  - nats://127.0.0.1:14223
dispatch_project: LEGSMOKE
repos:
  - sjawhar/legion-smoke
state_dir: $STATE
omp_invocation: mise x github:sjawhar/oh-my-pi@18.1.15-sami.20260908-220934 -- omp
omp_launch_prefix:            # copy the production daemon's list from /home/ubuntu/.config/legion/sjawhar-legion/legion.yaml verbatim
  - env
  - OMP_PROFILE=legion
  - secrets
  - ANTHROPIC_API_KEY
  - GEMINI_API_KEY
  - OPENAI_API_KEY
  - --
gates:
  design: root-issues
github_apps:
  implement:
    app_id: "3202636"
    private_key_command: 'printf "%s" "\$GH_AGENT_APP_PRIVATE_KEY_B64" | base64 --ignore-garbage --decode'
  review:
    app_id: "3202653"
    private_key_command: 'printf "%s" "\$GH_REVIEW_APP_PRIVATE_KEY_B64" | base64 --ignore-garbage --decode'
EOF

# 1. A pane opened by hand on the scratch daemon's own private server. `keepalive` keeps the
#    server alive after the worker pane dies, so the daemon's kill-pane sees "can't find pane"
#    (the production shape) rather than "no server running" (also pane-gone; see PANE_GONE_STDERR).
tmux -L legion-sjawhar11 new-session -d -s legion-sjawhar11 -n keepalive 'sleep 7200'
read -r WIN PANE < <(tmux -L legion-sjawhar11 new-window -d -t legion-sjawhar11 -n dead-worker -P -F '#{window_id} #{pane_id}' 'sleep 7200')

# 2. State with one ready-confirmed worker claim pointing at that pane and a nonexistent socket.
cat >"$SMOKE/seed-state.ts" <<'EOF'
const [srcRoot, stateDir, windowId, paneId] = process.argv.slice(2);
const { newLegionState, saveState } = await import(`${srcRoot}/packages/daemon/src/daemon/legion-state.ts`);
const { roleToken } = await import(`${srcRoot}/packages/contracts/src/legion-roles.ts`);
const project = "sjawhar11";
const token = roleToken(project, "LEGSMOKE-11", "tester");
const state = newLegionState(project, 4);
state.roles[token] = {
  issue: "LEGSMOKE-11",
  role: "tester",
  generation: 1,
  sessionId: "ses_dead_tester",
  readyConfirmedAt: Date.now(),
  locator: {
    tmuxSession: `legion-${project}`,
    tmuxWindowId: windowId,
    tmuxPaneId: paneId,
    socketPath: `${stateDir}/workers/dead-tester.sock`,
    ompSessionFile: `${stateDir}/workers/dead-tester.session.json`,
  },
};
await saveState(`${stateDir}/state.json`, state);
console.log(token);
EOF
TOKEN=$(cd "$SRC" && bun run "$SMOKE/seed-state.ts" "$SRC" "$STATE" "$WIN" "$PANE")
mkdir -m 700 -p "$STATE/secrets" && (umask 077; printf 'stale-boot-token' >"$STATE/secrets/$TOKEN")

# 3. Kill the pane, then boot the scratch daemon from SRC.
tmux -L legion-sjawhar11 kill-pane -t "$PANE"
(cd "$SRC" && secrets DISPATCH_TOKEN GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64 -- \
  env DISPATCH_URL="$DISPATCH_URL" LEGION_DAEMON_PORT=19371 \
  bun run packages/daemon/src/cli/index.ts start sjawhar/11 --config "$SMOKE/$R.yaml" >"$SMOKE/$R/daemon.log" 2>&1 &)
until curl -sf http://127.0.0.1:19371/legion/v1/state >/dev/null; do sleep 1; done   # boot: OMP probes + App token, ~20-60s

# 4. Evidence.
LEGION_DAEMON_URL=http://127.0.0.1:19371 legion state | jq ".roles[\"$TOKEN\"]" | tee "$SMOKE/$R/legion-state.json"
grep -n -E 'worker reconnection failed|TypeError|failed to reconnect worker' "$SMOKE/$R/daemon.log" | tee "$SMOKE/$R/log-lines.txt"
jq ".roles[\"$TOKEN\"]" "$STATE/state.json" | tee "$SMOKE/$R/state-json.json"
stat "$STATE/secrets/$TOKEN" 2>&1 | tee "$SMOKE/$R/secret-file.txt"

# 5. Teardown (scratch only).
pkill -f "cli/index.ts start sjawhar/11" ; tmux -L legion-sjawhar11 kill-server
```

Expected on **main**: `log-lines.txt` has `[legion] failed to reconnect worker <token>: …`
followed by `[legion] worker reconnection failed:` and `TypeError: undefined is not an object
(evaluating 'api.revokeSessionCapability')`; `legion-state.json` and `state-json.json` show
`locator` present; `stat` succeeds. Expected on **branch**: `log-lines.txt` has only the
`failed to reconnect worker` verdict line; `legion-state.json` shows the role with `sessionId`,
`readyConfirmedAt`, no `locator`; `state-json.json` shows no `locator` and
`resumeSessionFile: "<STATE>/workers/dead-tester.session.json"`; `stat` reports
`No such file or directory`. Also confirm `docker ps` never shows anything but `legion11-nats` being
touched and `tmux -L legion-sjawharlegion list-panes -a` is identical before and after.

**PR body E2E line:** `scratch daemon (project sjawhar/11, state /tmp/legion11-smoke/branch/daemon,
tmux -L legion-sjawhar11) — booted <branch head sha> with a ready-confirmed claim on a killed pane:
legion state shows the role without locator, no "worker reconnection failed" line, secrets/<token>
removed. Negative control: same steps on main (<main commit>) → TypeError at
api.revokeSessionCapability, locator and secret file retained.` Leave the rig at
`/tmp/legion11-smoke` for the reviewer.

Then `docker rm -f legion11-nats`, write `.legion/test.json` (verdict, head sha, artifact paths),
commit it, push, `legion handoff complete`.

---

## Spec coverage (self-review)

| spec line | task |
| :--- | :--- |
| Acceptance 1 (new `index.test.ts` test; red on main, green with fix; run command) | Task 1 Steps 2–3, Task 2 Step 4 |
| Acceptance 2 (`bun test packages/daemon`, biome, tsc clean) | Task 4 Step 1 |
| Acceptance 3 (scratch daemon, main first, `legion state`, no failure line, secret gone) | §Tester |
| Acceptance 4 (PR states what moved + closure audit) | Task 4 Step 3 |
| Requirement: revoke never skipped / no `if (api)` | Task 2 (pure move; no guard) |
| Requirement: all four closures unreachable before `api` | Closure audit; Task 2 Step 3 comment |
| Requirement: probe finishes before admission | Task 2 (awaited, before `enableWorkerPromotion()`) |
| Requirement: `reconnectWorkers` unchanged | Task 2 (byte-for-byte move) |
| Requirement: production daemon never restarted | §Tester, Global constraints |
| Design 1–2 (what moves, where) | Task 2 Steps 1–3 |
| Design 3 (comment replaced with the real rule) | Task 2 Step 3 |
| Design 4 (API live during probe is safe) | Task 2 Step 3 comment; AGENTS.md bullet |
| Design 5 (no behaviour change in `processes.ts`/`api.ts`/`api/auth.ts`; stale comments updated) | Task 3 |
| Errors table | pinned by Task 1's assertions (locator cleared, `resumeSessionFile` kept, kill-pane pane-gone accepted, file removed) |
| Testing row 2 (three `processes.test.ts` cases pass unchanged) | Task 4 Step 1 |
| Rejected list | Global constraints |

## Plan verification (planner, 2026-09-12)

Tasks 1–2 were applied byte-for-byte from this document to a throwaway `git archive` of the branch
parent `7db2cd83` (`/tmp/legion11-plan-check`, since removed; the issue workspace was not touched):

- Task 1 alone: `bun test … index.test.ts -t "retires a ready-confirmed worker"` → **1 fail** at
  `expect(failures).toEqual([])`, received
  `[["[legion] worker reconnection failed:", TypeError: undefined is not an object (evaluating 'api.revokeSessionCapability')]]`.
- Task 1 + Task 2: `index.test.ts` **19 pass / 0 fail**; the three `processes.test.ts` reconnect
  cases **3 pass** unmodified; `bunx biome check` on both edited files exit 0;
  `bunx tsc --noEmit` in `packages/daemon` exit 0.
- §Tester's `seed-state.ts` run against that copy wrote a `state.json` that the real `loadState`
  accepted (`version 23`, claim with locator, `readyConfirmedAt`, `sessionId`), token
  `legion-sjawhar11-legsmoke-11-tester`.
