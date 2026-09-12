# Heartbeat role re-assertion — implementation plan (LEGION-29)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (one
> implementer, one PR against `sjawhar/legion`, branch `legion/LEGION-29`). Steps use `- [ ]` for
> tracking. Every ruling from a review round supersedes this document.

**Goal:** A session that holds an Envoy role keeps holding it without human action: when the
listener no longer resolves the session as the role's live holder, the session's own registration
heartbeat re-asserts the claim within one interval, and a Legion controller or root architect that
regains its role re-runs its daemon ready call so held notices drain and missed wakes replay.

**Architecture:** `packages/pi-envoy/extensions/envoy.ts` gains `reassertRole`, run by the
heartbeat tick after every successful `registerSession()`: `GET /v1/roles/<role>`; if the listener
does not name this session, a soft `POST /v1/roles/set`; the listener's soft-claim rule decides
(unheld / dead holder → success; a different live holder → 409, which ends re-assertion). A regain
fires a "role regained" hook on the existing process-wide `LEGION_ROLE_CLAIM_BRIDGE` symbol.
`extensions/legion.ts` registers that hook: controller → `POST /legion/v1/controller/ready`
again; root architect → `POST /legion/v1/process/ready` again; phase worker → nothing (the daemon's
own no-holder recovery already prompts a live worker's catch-up). No listener (Go) change, no
daemon change.

**Tech stack:** TypeScript on Bun; `@legion/envoy-client/transport` (`createEnvoyClient`,
`EnvoyApiError`); Bun test with the existing `createPi`/`sessionContext`/`setInterval`-capture
fixtures; Biome; jj.

**Spec:** `dispatch://LEGION-29/artifact/spec` (version 2). Read it first; it supersedes the
issue's original text.

## Decisions needed

None from Sami. One refinement below (R1) goes beyond the spec's letter; the architect vetoes it
before Task 2 starts or it stands.

## Refinements to the spec (read before implementing)

1. **R1 — the regain hook also fires on the first healthy heartbeat after a registry outage, with
   reason `"reregistered"`, even when the claim itself survived.** The spec's data flow fires the
   hook only when a soft claim lands after `GET /v1/roles/<role>` reports the role unheld. That
   cannot see tonight's most common hole: the listener restarts, the outage outlasts the
   `envoy_sessions` TTL (5 min; both 05:34→05:42 and 07:25→07:34 did), and on restart the durable
   `envoy_roles` claim is intact but `liveRoleHolder` answers "no holder" for every session whose
   registry entry lapsed — until that session's next heartbeat re-registers. The daemon's
   controller-kind publishes in that window 404 and land in `controllerPendingNotices`
   (`events.ts:479-485`), and nothing but `/controller/ready` drains them (`index.ts:625-628`).
   When the session finally re-registers, the GET already names it (its own registration just
   restored the entry), so the spec's hook would never fire and the notices would sit until a human
   re-claims — exactly the "8 held notices" the controller drained by hand at 07:42. The heartbeat
   already knows a tick failed (`heartbeatOutageNotified`); the first success after that is the
   moment to re-run the ready call. Cost: one idempotent `/controller/ready` (drain + forced resync)
   or `/process/ready` (overseer catch-up) per observed outage. Acceptance 6's
   "`controllerPendingNotices: 0` after the drain" is only reachable with this. Steady state stays
   quiet (no claim, no transcript entry — Acceptance 2 is unaffected).
2. **A 409 writes no transcript entry.** The spec says a 409 "drops the local claim, warns once,
   stops re-asserting". The drop is `claimedRoleTopic = undefined` only — no
   `pi.appendEntry(ROLE_CLAIM_ENTRY, { role: null })` — mirroring the existing refused-soft-reclaim
   path in `setEnvoyRole` (`envoy.ts:390-398`). A later `omp --resume` therefore soft-reclaims from
   the transcript and the listener arbitrates again (409 if the newer holder is still live, success
   if it died). Recording a release would make that resume give up a role nobody holds.
3. **The `GET` decides nothing on its own.** When the GET names a *different* holder, the
   heartbeat still issues the soft claim and lets the listener's atomic rule answer (409, or
   success if that holder died between the two calls). One arbiter, one code path.

## Global constraints

- Scope is `packages/pi-envoy/extensions/envoy.ts`, `extensions/legion.ts`, their two test files,
  `packages/pi-envoy/AGENTS.md`, and one sentence in `skills/legion-controller/SKILL.md`. No change
  under `packages/envoy/` (Go) or `packages/daemon/`. `legion.minPluginVersion` is not raised.
- Wire shapes (from `packages/envoy-client/src/transport.ts`): `GET /v1/roles/<role>` → 200
  `{ role, holder, last_seen }` or 404 `{ error }` (`request()` throws `EnvoyApiError` with
  `details.status === 404`); `POST /v1/roles/set` body is exactly `{ session_id, role }` plus
  `soft: true` when soft, plus `previous_session_id` only when set (`transport.ts:334-345`);
  `client.setRole` returns `{ claimed: false, holder }` on 409 instead of throwing
  (`transport.ts:349-355`).
- A re-assertion never sends `previous_session_id` (the spec rejects it: a same-id soft claim
  already succeeds).
- `pi.appendEntry(ROLE_CLAIM_ENTRY, …)` is written only by `setEnvoyRole` (a claim that lands) and
  `releaseClaimedRole`. `reassertRole` never calls it.
- Every heartbeat failure path stays inside the existing tick promise chain
  (`ensureHeartbeat`, `envoy.ts:315-350`): warn once per outage via `context.ui.notify`, retry next
  tick, never reject unhandled (OMP treats an unhandled rejection as fatal).
- Repo rules: `node:` imports, Biome (double quotes, semicolons, 100 cols), `type` imports, no
  barrel files, comments describe current behaviour, jj not git. Run `bunx biome check`,
  `bunx tsc --noEmit`, `bun test` per package only in Task 6; never `| head` / `| tail` on gate
  commands.

---

## Part A — Forensics (does not gate the fix)

Evidence gathered 2026-09-12 08:10–08:30Z on this host (`docker ps -a`, `docker inspect`,
`docker logs --since 2026-09-12T05:00:00Z --until 2026-09-12T08:00:00Z envoy-listener`, `git log -S`,
`git show <deployed-commit>:…`). Full listener log: `/tmp/legion29-forensics/listener.log` (18,521
lines); keyword filter: `/tmp/legion29-forensics/listener-filtered.log`.

### Deployed image

| fact | value |
| :--- | :--- |
| container | `envoy-listener`, host network, `PORT=9020`, `NATS_URLS=nats://envoy-nats:4222`, started by `deploy/scripts/up-listener.sh` (`docker run`, no compose labels) |
| image tag | `ghcr.io/sjawhar/legion/envoy:fcd13a145853be600b4770d2d908dfc51396d767` |
| image digest | `sha256:e99c507943d208684612bbb4af09396b9107bc3e9f1b0c5a4102be8696c88815`, built `2026-09-10T00:14:43Z` |
| built from | commit `fcd13a14` — `fix(pi-envoy): a held Envoy role survives resume and follows a fork (#832)`, 2026-09-09T18:09:07-06:00 |
| `RestartCount` | 3 (in-place restarts of the same image; `StartedAt=2026-09-12T08:04:55Z`, `FinishedAt=2026-09-12T07:58:57Z`) |
| `RoleBucket = "envoy_roles"` in the deployed binary | **yes** — introduced `2d4a9c8e` (`feat(envoy): add role-based routing with envoy_role_set (#401)`, 2026-04-10); `git show fcd13a14:packages/envoy/internal/store/kv.go` has `RoleBucket`, `SetRole`, `RoleHolder`, `releaseRoleClaim`, `releaseAllRoleClaims` |
| `ReleaseExpiredRoleClaim` / `ReapRoleClaims` / `StartRoleClaimReaper` in the deployed binary | **no** — introduced `95b93e0b` (`fix(envoy): make role recovery and watcher health durable (#958)`, merged 2026-09-12T07:09:41Z, inside the incident window), never rebuilt/redeployed |

**Acceptance 6 precondition is satisfied**: the deployed image contains `envoy_roles`. Record the
tag and digest above in the tester's E2E line. Note the deployed read path differs from `main`: at
`fcd13a14`, `liveRoleHolder` (`cmd/listener/api.go`) returns "no holder" when the holder's
`envoy_sessions` entry is missing but does **not** delete the claim; `main`'s
`ReleaseExpiredRoleClaim` (`kv.go:416-438`) deletes it unless the claim was restored at process
open and one session TTL has not yet elapsed.

Production NATS is not a container on this host: `envoy-nats` resolves via Tailscale to
`100.127.163.46:4222` (`envoy-nats.tailb86685.ts.net`, NATS 2.10.29, confirmed live by a raw
`INFO` banner). No `nats` CLI is installed and `natsio/nats-box` could not be pulled, so
`nats kv info envoy_roles` / `envoy_sessions` and NATS server logs were **not** inspected. Residual
gap; nothing in the listener log points at NATS-side data loss (no bucket/stream recreation, only
`nats: timeout` / `interest kv: context deadline exceeded` / reconnect lines around each
self-termination).

### Timeline 05:00–08:00Z (listener log)

| time (Z) | event |
| :--- | :--- |
| 05:03:28–05:18:28 | `reaper cycle reaped=0` every 5 min |
| 05:23:28 | `reaper cycle reaped=6` |
| 05:28:28 | `reaper cycle reaped=1` |
| 05:32:28–05:34:06 | ~40 × `listener upsert failed … nats: timeout`; 05:33:19 `listener session registry put failed` ×4; 05:33:20/05:33:40 `self-health probe failed` (1/3, 2/3) |
| 05:33:41 | `reaper cycle failed error="context deadline exceeded"` |
| 05:34:22 | `envoy-listener shutdown complete` (restart 1) |
| 05:41:56 / 05:42:02 | `envoy-listener listening` / `ready (NATS connected)` — outage ≈ 7.5 min |
| 05:47:03 | first post-restart `reaper cycle reaped=0` |
| 06:02:03–07:17:03 | reaper cycles reaping 1, 3, 4, 5, 1, 1, 1, 1, 2, 4, 6, 1, 1, 1 sessions (listener up and healthy) |
| 07:22:48 / 07:24:51 / 07:25:46 | `self-health probe failed` 1/3 → 3/3 `interest kv: context deadline exceeded`; 07:25:46 `self-health threshold exceeded — terminating` |
| 07:25:46 | `shutdown complete` (restart 2) |
| 07:34:40 / 07:34:47 | `listening` / `ready` — outage ≈ 8.9 min (the restart the controller reported) |
| 07:39:47–07:54:47 | `reaper cycle reaped=0` ×4 |
| 07:58:55 | `session registry watcher stopped`, `nats: consumer not active`, `envoy nats disconnected`; 07:58:56 `shutdown complete` (restart 3; next start 08:04:55, outside the window) |

No line in the window mentions `envoy_roles`, `ReleaseExpiredRoleClaim`, or any role-claim event:
the deployed binary logs none. The reaper logs a count, not session ids, so **which** sessions'
claims were deleted is not recoverable from the log.

### Which path dropped tonight's claims

- **(a) deployed image predates `envoy_roles` — no.** The bucket is five months older than the
  image.
- **(b) a claim deleted after the holder's `envoy_sessions` entry lapsed while the listener stayed
  up — yes, through the deployed image's code, not through `ReleaseExpiredRoleClaim` (which it
  lacks).** At `fcd13a14` the only claim-cleanup path is the interest reaper:
  `StartReaper(isAlive, 5*time.Minute, 10*time.Minute)` in `cmd/listener/main.go` →
  `Registry.Reap()` marks every cached session stale when `isAlive(sid)` is false **and** the
  interest row's `UpdatedAt` is more than 10 min old, then calls `r.Remove(sid, nil)` → which, with
  no topics, calls `r.releaseAllRoleClaims(sid)` — every `envoy_roles` key whose value is that
  session id is deleted. `isSessionLive` is `sessions.Get(sid) == nil`, so **any** KV read error
  (the `nats: timeout` / `context deadline exceeded` storms at 05:2x–05:33 and 07:2x) counts every
  session as dead, and an interest row goes stale as soon as its upserts fail for 10 minutes
  (`listener upsert failed` ran for minutes before each self-termination). The 05:23:28 (6) and
  05:28:28 (1) reaps fall exactly in the first degraded window; 06:02–07:17 reaped 33 more with the
  listener up. Listener restart alone is *not* a permanent loss on this image: after 07:34:40 the
  claims still in KV resolved again as each holder's next heartbeat re-registered it
  (`liveRoleHolder` at `fcd13a14` never deletes); the controller's 07:41 "no holder" reads are
  consistent with either that transient window (up to one 120 s heartbeat per session) or a prior
  reaper deletion — the log cannot tell which for a given role.
- **(c) NATS data loss — not supported.** Only transient client-side timeout/reconnect lines; the
  post-restart reaper cycles reaped 0 (the interest cache repopulated from KV), and role deliveries
  to other architect topics continued after 07:34:47. Off-host NATS logs were not available (gap
  noted above).

`main` (PR #958) already replaces both halves — `Reap()` now calls `deleteInterest(sid)` and never
touches roles; a dedicated `StartRoleClaimReaper(isAlive, 5*time.Minute, sessions.TTL())` and the
read-path `ReleaseExpiredRoleClaim` give a restored claim one session TTL — but it is **not
deployed**. Rolling `ENVOY_IMAGE_TAG` forward is an operational item for the architect
(`packages/envoy/deploy/README.md`), outside this PR. Independently of the listener version, a
session on today's plugin never re-asserts a lost claim; that is what this plan fixes, and it also
bounds the hole for any future listener regression to one heartbeat.

### Rollout note for the architect

Two plugin roots are live on this box: `~/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy`
(version 1.4.0, default profile) and
`~/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion-envoy` (version 1.3.0 — the
`legion` OMP profile this planner's own pane runs under). The fix reaches Legion panes only when the
`legion` profile's pin moves, not just the default one.

---

## Part B — Daemon behaviour on a no-holder 404 (Acceptance 5 finding)

Read with the daemon at `main` (`7add4d03`). Line numbers are exact at that commit.

| role kind | what the daemon does when its publish to the role 404s | catch-up delivered to a *live* session? | on regain the session must… |
| :--- | :--- | :--- | :--- |
| controller | HTTP 404 → `publishOrThrow` pushes the payload onto `state.controllerPendingNotices` (all controller payloads but `triage`) and calls `notifyUndeliverable` (`events.ts:470-489`, `414-435`) → `onUndeliverable` (`index.ts:517-531`) → `processManager.ensureController()` (`processes.ts:1396-1418`): the controller pane is alive and `state.roles[controllerToken]` is still set (daemon state is separate from Envoy's KV), so it only cancels the registration deadline and returns. Nothing drains the notices: only `/controller/ready` does (`handleControllerReady`, `api/routes/controller.ts:11-28` → `onControllerReady`, `index.ts:625-628`: `drainControllerNotices()` then `emitResync({ force: true })`). | **no** | call `POST /legion/v1/controller/ready` again with `{ secret, sessionId }`. Repeat-safe: it rewrites the same `roles[controllerToken]` claim, saves, best-effort `markControllerReady`, drains, resyncs — `/legion-claim-controller` already re-runs it by hand (`legion.ts:249-262`). |
| root architect | `onUndeliverable` → `parseRoleToken` → `rootForIssue` → `processManager.resumeWorker(root, tree, "architect")` (`index.ts:523-527`). The root's claim is `{ issue, role, sessionId, agentId }` (`api/routes/process.ts:47-52`) — no `locator`, no `resumeSessionFile` — so `resumeWorker` hits its no-op guard (`processes.ts:1841-1846`, logs `resumeWorker no-op … never spawned, or already fully retired`) and returns. The **exceptions lane** is different: a listener `no_holder`/`delivery_failed` exception (`exceptionInfo`, `events.ts:296-325`) → `handleException` (`processes.ts:1795-1820`) probes the root and, if alive, sends the `reclaim-architect` control directive with the original payload as `redeliver` (`1806-1812`; `controlDirective`, `1707-1734`), which `legion.ts` answers with a hard `claimEnvoyRole` (`reclaimArchitect`, `legion.ts:264-269`). That lane only fires for deliveries that failed inside the listener's core-NATS role lane, never for the daemon's own HTTP 404. | **no** (durable/HTTP lane); yes only via the exceptions lane | call `POST /legion/v1/process/ready` again with `{ tree, sessionId, secret, generation }`. Repeat-safe: `handleProcessReady` (`api/routes/process.ts:71-96`) 409s only on a generation mismatch (`79-81`, correct for a superseded pane); `confirmRootReady` (`processes.ts:1503-1509`) re-sets `readyConfirmedAt`/`launchFailures = 0` with no already-confirmed guard; `onTreeReady` → `emitOverseerCatchup` (`index.ts:509-515`, wired at `616`) publishes a fresh state-derived catch-up to the architect role — the documented recovery for a root's missed wake (`markTreeReady` doc, `processes.ts:1746-1750`); `markTreeReady` → `workerClient` returns the cached client (`2242-2244`). |
| phase worker (incl. sub-architect) | `onUndeliverable` → `resumeWorker` → claim has a `locator` → `workerCatchup(...)` → `spawnWorker(root, issue, role, JSON.stringify(catchup))` (`processes.ts:1847-1849`). With `locator`, `sessionId`, `readyConfirmedAt` set, `spawnWorker` probes the socket (`539-543`); a connected client goes to `resumeOrQueueExisting` (`565-574`), which prompts the catch-up directly on an idle worker below the cap or queues it for the next idle transition. `/worker/ready` on a confirmed claim is a no-op (`workerReady`, `692-743`: `readyConfirmedAt !== undefined` → `cancelBootWatchdog`, return, `720-723`; `handleWorkerReady`, `api/routes/workers.ts:273-294`). | **yes** | nothing. Re-running `/worker/ready` would deliver nothing; the daemon's own recovery already prompted or queued the catch-up. |

---

## File structure

| file | change |
| :--- | :--- |
| `packages/pi-envoy/extensions/envoy.ts` | import `EnvoyApiError`; bridge gains `regained` slot + exported `onEnvoyRoleRegained` and `RoleRegainReason`; new `reassertRole`; `ensureHeartbeat` tick runs it after `registerSession()` |
| `packages/pi-envoy/extensions/envoy.test.ts` | extend the `role-claim-heartbeat` test's fetch mock; four new tests (re-assert on unheld; quiet steady state; 409 ends re-assertion; outage → `reregistered`) |
| `packages/pi-envoy/extensions/legion.ts` | import `onEnvoyRoleRegained`; `controllerRoleToken`; the regain listener (controller → `controllerReady`, root → `processReady`, worker → no-op) |
| `packages/pi-envoy/extensions/legion.test.ts` | `bootWorker` gains an `intervals` option; three new tests (controller, root architect, phase worker) |
| `packages/pi-envoy/AGENTS.md` | one Critical-conventions bullet |
| `skills/legion-controller/SKILL.md` | one sentence in "Start and claim the controller role" |

---

## Task 1: `envoy.ts` — re-assert a lost claim from the heartbeat; 409 ends it; steady state quiet

**Files:**
- Modify: `packages/pi-envoy/extensions/envoy.ts:28` (import), `:66-107` (bridge), `:315-350`
  (`ensureHeartbeat`), insert `reassertRole` immediately before `ensureHeartbeat`
- Test: `packages/pi-envoy/extensions/envoy.test.ts` — modify the test at `:1728-1786`, add three
  tests after it

**Interfaces:**
- Produces (consumed by Task 2–4):
  ```ts
  export type RoleRegainReason = "reclaimed" | "reregistered";
  export function onEnvoyRoleRegained(
    listener: (role: string, reason: RoleRegainReason) => Promise<void>
  ): void;
  ```
  `role` is the bare role token (e.g. `legion-omp-controller`), never the `notifications.role.`
  topic. Last registration wins (single slot on the bridge, like `claim`).

- [ ] **Step 1: Extend the existing heartbeat test so it answers the new GET**

In `envoy.test.ts`, test `"a role claim keeps the existing registration fresh"` (`:1728`), the fetch
mock's fallback answers `GET /v1/roles/legion-controller` with `{}`, which the new code would fail
to parse and route into the warn-once path. Add a branch before the `/v1/interests/subscribe` one:

```ts
      if (url.pathname === "/v1/roles/legion-controller") {
        return response({ role: "legion-controller", holder: "ses_role_heartbeat", last_seen: 1 });
      }
```

- [ ] **Step 2: Write the failing tests**

Insert after that test (after `:1786`):

```ts
  test("a heartbeat tick re-asserts a held role the listener no longer holds, softly and without a transcript entry", async () => {
    const role = "legion-controller";
    let listenerHoldsClaim = true;
    const roleClaims: Record<string, unknown>[] = [];
    const roleReads: string[] = [];
    const reasserted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads.push(url.pathname);
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
        }
        return response({ role, holder: "ses_reassert", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        roleClaims.push(body);
        listenerHoldsClaim = true;
        if (body.soft === true) reasserted.resolve();
        return response({
          session_id: "ses_reassert",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-reassert"
    );
    const regained: { readonly role: string; readonly reason: string }[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_reassert"),
      setInterval: (callback) => intervals.push(callback),
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    await roleTool.execute("", { role });
    expect(roleClaims).toEqual([{ session_id: "ses_reassert", role }]);

    // The listener drops the claim out from under the session (a reaped claim, NATS data
    // loss, an older listener build). Nothing in this process notices until the heartbeat.
    listenerHoldsClaim = false;
    intervals[0]?.();
    await reasserted.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(roleReads).toEqual([`/v1/roles/${role}`]);
    expect(roleClaims).toEqual([
      { session_id: "ses_reassert", role },
      { session_id: "ses_reassert", role, soft: true },
    ]);
    // The held role did not change, so the durable record is not rewritten.
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);
    expect(regained).toEqual([{ role, reason: "reclaimed" }]);
  });

  test("heartbeat ticks while this session is the live holder issue no claim and append no transcript entry", async () => {
    const role = "legion-controller";
    const roleClaims: unknown[] = [];
    let roleReads = 0;
    let registrations = 0;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads += 1;
        return response({ role, holder: "ses_quiet", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_quiet",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") registrations += 1;
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-quiet"
    );
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_quiet"),
      setInterval: (callback) => intervals.push(callback),
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    const claimEntries = () =>
      fixture.entries.filter((entry) => entry.customType === "envoy-role-claim");
    expect(claimEntries()).toHaveLength(1);
    expect(registrations).toBe(1);

    for (let tick = 0; tick < 3; tick += 1) {
      intervals[0]?.();
      await new Promise((resolve) => setTimeout(resolve, 10));
    }

    expect(registrations).toBe(4);
    expect(roleReads).toBe(3);
    expect(roleClaims).toEqual([{ session_id: "ses_quiet", role }]);
    expect(claimEntries()).toHaveLength(1);
    expect(regained).toEqual([]);
  });

  test("a 409 on re-assertion drops the local claim, warns once, and ends re-assertion for that role", async () => {
    const role = "pr-queue";
    const roleClaims: Record<string, unknown>[] = [];
    let roleReads = 0;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads += 1;
        // A newer live session took the role (a fork child, a second controller).
        return response({ role, holder: "ses_newer", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        roleClaims.push(body);
        if (body.soft === true) {
          return Response.json(
            { error: `role ${role} is held by ses_newer`, role, holder: "ses_newer" },
            { status: 409 }
          );
        }
        return response({
          session_id: "ses_refused",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-409"
    );
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const refused = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_refused"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        notify: (message) => {
          notifications.push(message);
          if (message.includes("is now held by")) refused.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    intervals[0]?.();
    await refused.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));
    // Two more ticks: re-registration continues, re-assertion of this role does not.
    for (let tick = 0; tick < 2; tick += 1) {
      intervals[0]?.();
      await new Promise((resolve) => setTimeout(resolve, 10));
    }

    expect(roleClaims).toEqual([
      { session_id: "ses_refused", role },
      { session_id: "ses_refused", role, soft: true },
    ]);
    expect(roleReads).toBe(1);
    expect(notifications.filter((message) => message.includes("is now held by"))).toEqual([
      `envoy: role ${role} is now held by session ses_newer; this session no longer holds it`,
    ]);
    expect(regained).toEqual([]);
    // The local claim is gone: an unsubscribe of "everything" finds no role left to release …
    const unsubscribe = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const released = await unsubscribe?.execute("", {});
    expect(released?.content[0]?.text).toBe("Unsubscribed: (none)");
    // … and no release entry was written: the 409 mirrors setEnvoyRole's refused soft reclaim,
    // so a later resume still lets the listener arbitrate from the transcript.
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);
  });
```

- [ ] **Step 3: Run the new tests and confirm they fail**

Run (from `packages/pi-envoy`):
`bun test extensions/envoy.test.ts -t "re-asserts a held role|live holder issue no claim|409 on re-assertion"`
Expected: FAIL — `onEnvoyRoleRegained is not a function` (first two), and no soft claim / no
notification (third).

- [ ] **Step 4: Implement**

`envoy.ts:28` — add the error class to the transport import:

```ts
import {
  createEnvoyClient,
  EnvoyApiError,
  expandSubscriptionTopics,
} from "@legion/envoy-client/transport";
```

`envoy.ts:66-107` — replace the bridge block with:

```ts
type LegionRoleClaim = (sessionID: string, role: string, context?: SessionContext) => Promise<void>;

/**
 * Why the heartbeat decided the listener had lost sight of this session's role: `"reclaimed"` —
 * the listener no longer named this session and a soft claim landed; `"reregistered"` — the
 * claim itself survived, but this session had been unreachable (a registry outage), so the
 * listener may have answered "no holder" for it meanwhile.
 */
export type RoleRegainReason = "reclaimed" | "reregistered";

type LegionRoleRegained = (role: string, reason: RoleRegainReason) => Promise<void>;

type LegionRoleClaimReady = {
  readonly promise: Promise<LegionRoleClaim>;
  readonly resolve: (claim: LegionRoleClaim | PromiseLike<LegionRoleClaim>) => void;
};

type LegionRoleClaimBridge = {
  claim: LegionRoleClaim | undefined;
  readonly ready: LegionRoleClaimReady;
  /** legion.ts's regain hook. One slot, like `claim`: one Legion extension instance per process. */
  regained: LegionRoleRegained | undefined;
};

interface GlobalLegionRoleClaimBridgeStore {
  [key: symbol]: LegionRoleClaimBridge | undefined;
}

// A process-wide symbol bridges legion.ts's `claimEnvoyRole` import to the
// one envoyExtension(pi) instance OMP actually ran, since each manifest entry
// loads as its own module instance with its own module-scope state.
const LEGION_ROLE_CLAIM_BRIDGE = Symbol.for("legion.pi-envoy.role-claim-bridge");

function legionRoleClaimBridge(): LegionRoleClaimBridge {
  const store = globalThis as typeof globalThis & GlobalLegionRoleClaimBridgeStore;
  const bridge = store[LEGION_ROLE_CLAIM_BRIDGE];
  if (bridge) return bridge;

  const createdBridge: LegionRoleClaimBridge = {
    claim: undefined,
    ready: Promise.withResolvers<LegionRoleClaim>(),
    regained: undefined,
  };
  store[LEGION_ROLE_CLAIM_BRIDGE] = createdBridge;
  return createdBridge;
}

export async function claimEnvoyRole(
  sessionID: string,
  role: string,
  context?: SessionContext
): Promise<void> {
  const bridge = legionRoleClaimBridge();
  await (bridge.claim ?? (await bridge.ready.promise))(sessionID, role, context);
}

/**
 * Registers the hook the heartbeat fires after it re-establishes this session as `role`'s live
 * holder (see `reassertRole`). legion.ts re-runs the role's daemon ready call from it. Last
 * registration wins, exactly like `claimEnvoyRole`'s bridge slot.
 */
export function onEnvoyRoleRegained(
  listener: (role: string, reason: RoleRegainReason) => Promise<void>
): void {
  legionRoleClaimBridge().regained = listener;
}
```

Insert immediately before `const ensureHeartbeat = …` (`envoy.ts:315`):

```ts
  /**
   * Heartbeat follow-up: make sure the listener still resolves this session as the live holder
   * of `claimedRoleTopic`. The listener can lose sight of a live holder without this process
   * noticing — a claim reaped after this session's registry entry lapsed, NATS data loss, an
   * older listener build — and until now that lasted until a human re-ran envoy_role_set. Reads
   * first (`GET /v1/roles/<role>`), so a healthy tick writes nothing; a soft claim goes out only
   * when the listener does not name this session, and the listener's soft-claim rule is the
   * arbiter: a 409 (a different live holder) ends re-assertion for good — the newer holder is
   * correct, so the local claim is dropped exactly as a refused automatic reclaim drops it
   * (`setEnvoyRole`), with no transcript entry either way. A regain (`"reclaimed"`), or the
   * first healthy tick after a registry outage during which the listener may have answered
   * "no holder" for a claim that survived (`"reregistered"`), fires legion.ts's hook so the
   * role's daemon ready call runs again. Errors propagate to the heartbeat's warn-once path.
   */
  const reassertRole = async (afterOutage: boolean, context: SessionContext): Promise<void> => {
    const topic = claimedRoleTopic;
    if (topic === undefined) return;
    const role = topic.slice(ROLE_TOPIC_PREFIX.length);
    const holder = await client.getRole(role).then(
      (info) => info.holder,
      (error: unknown) => {
        // 404 is the listener's answer for "no live holder", not a failure.
        if (error instanceof EnvoyApiError && error.details.status === 404) return undefined;
        throw error;
      }
    );
    let reason: RoleRegainReason;
    if (holder === sessionID) {
      if (!afterOutage) return;
      reason = "reregistered";
    } else {
      const result = await client.setRole({ sessionID, role, soft: true });
      if (claimedRoleTopic !== topic) {
        // envoy_role_set or envoy_unsubscribe changed the claim while this was in flight; that
        // call is the truth, so a claim that landed here is handed straight back.
        if (result.claimed) await client.unsubscribe({ sessionID, topics: [topic] });
        return;
      }
      if (!result.claimed) {
        claimedRoleTopic = undefined;
        logger.warn("envoy: role re-assertion refused; held by another live session", {
          role,
          sessionID,
          holder: result.holder,
        });
        context.ui.notify(
          `envoy: role ${role} is now held by session ${result.holder}; this session no longer holds it`,
          "warning"
        );
        return;
      }
      logger.warn("envoy: role re-asserted after the listener lost the claim", {
        role,
        sessionID,
        previousHolder: holder ?? null,
      });
      reason = "reclaimed";
    }
    await legionRoleClaimBridge().regained?.(role, reason);
  };
```

In `ensureHeartbeat` (`envoy.ts:323-348`), replace the `context.setInterval(() => { … })` body's
dispatch with:

```ts
    context.setInterval(() => {
      // Sessions can be created lazily after session_start (a fresh TUI has no
      // session yet), and the ID this closure registered with goes stale. Heal
      // on drift instead of heartbeating a dead identity forever; until the
      // host mints an id there is nothing to register.
      const liveSessionID = context.sessionManager.getSessionId();
      if (liveSessionID === "") return;
      const drifted = liveSessionID !== sessionID;
      if (healing) return;
      healing = true;
      // A drifted id re-establishes the whole session (reclaimHeldRoles included); a steady one
      // re-registers, then checks the listener still resolves this session's role.
      const afterOutage = heartbeatOutageNotified;
      void (drifted
        ? establishSession(context)
        : registerSession().then(() => reassertRole(afterOutage, context))
      )
        .then(() => {
          heartbeatOutageNotified = false;
        })
        .catch((error) => {
          if (heartbeatOutageNotified) return;
          heartbeatOutageNotified = true;
          context.ui.notify(
            `envoy: registry heartbeat failed (${messageFor(error)}); retrying every heartbeat`,
            "warning"
          );
        })
        .finally(() => {
          healing = false;
        });
    }, defaults.heartbeatMs);
```

(`afterOutage` is consumed by Task 2; in this task it is always `false` on a healthy tick and the
`"reregistered"` branch is unreachable until Task 2's test.)

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `bun test extensions/envoy.test.ts` (whole file — the modified `role-claim-heartbeat` test and
the `heartbeat-blip` test must stay green).
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" describe -m "fix(pi-envoy): the registration heartbeat re-asserts a held role the listener lost

GET /v1/roles/<role> each tick; a soft claim only when the listener does not
name this session; a 409 ends re-assertion; no transcript entry on regain." && jj -R "$LEGION_WORKSPACE" new
```

---

## Task 2: `envoy.ts` — the first healthy heartbeat after an outage fires `"reregistered"` (R1)

**Files:**
- Modify: nothing new in `envoy.ts` (Task 1's `afterOutage` wiring already implements it)
- Test: `packages/pi-envoy/extensions/envoy.test.ts` — one new test after Task 1's

**Interfaces:** consumes `onEnvoyRoleRegained` / `RoleRegainReason` from Task 1.

- [ ] **Step 1: Write the failing test**

```ts
  test("the first healthy heartbeat after a registry outage fires the regain hook even though the claim survived", async () => {
    // A listener restart that outlasts the envoy_sessions TTL: the durable claim is intact, but
    // until this session re-registers the listener answers "no holder" for it, and the daemon's
    // publishes in that window went undelivered. The re-registration itself is the regain.
    const role = "legion-controller";
    let registryDown = false;
    const roleClaims: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      if (registryDown) throw new Error("network unreachable");
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        return response({ role, holder: "ses_outage", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_outage",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-outage-regain"
    );
    const regained: { readonly role: string; readonly reason: string }[] = [];
    const hooked = Promise.withResolvers<void>();
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
      hooked.resolve();
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_outage"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        notify: (message) => {
          notifications.push(message);
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    registryDown = true;
    intervals[0]?.();
    await warned.promise;
    // Let the failed tick's own `.finally` release the healing guard before the next tick.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(regained).toEqual([]);

    registryDown = false;
    intervals[0]?.();
    await hooked.promise;
    expect(regained).toEqual([{ role, reason: "reregistered" }]);
    // The claim survived: only the explicit hard claim ever went out.
    expect(roleClaims).toEqual([{ session_id: "ses_outage", role }]);

    // The following healthy tick is quiet again.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(regained).toHaveLength(1);
    expect(notifications.filter((message) => message.includes("registry heartbeat failed"))).toHaveLength(1);
  });
```

- [ ] **Step 2: Run it**

Run: `bun test extensions/envoy.test.ts -t "after a registry outage"`
Expected: PASS already if Task 1 was implemented verbatim (the `afterOutage` wiring is Task 1's).
If it fails, the `afterOutage` capture or the `"reregistered"` branch in `reassertRole` is wrong —
fix there, not in the test.

- [ ] **Step 3: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" describe -m "test(pi-envoy): the first healthy heartbeat after a registry outage reports a regain" && jj -R "$LEGION_WORKSPACE" new
```

---

## Task 3: `legion.ts` — a controller that regains its role re-runs `/controller/ready`

**Files:**
- Modify: `packages/pi-envoy/extensions/legion.ts:32` (import), `:199-200` (state), `:249-262`
  (`claimController`), insert the regain listener after `bootstrapWorker` (`:502`) and before
  `pi.on("session_start", …)` (`:504`)
- Test: `packages/pi-envoy/extensions/legion.test.ts` — one new test after
  `"claims the controller role at startup and on demand for an interactive session"` (`:731`)

**Interfaces:**
- Consumes: `onEnvoyRoleRegained(listener)` from Task 1.
- Produces: the listener body Task 4 extends; module state `controllerRoleToken: string | undefined`.

- [ ] **Step 1: Write the failing test**

```ts
  test("a controller that regains its role re-runs controller/ready so held notices drain", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const token = "legion-omp-controller";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_CONTROLLER_SECRET = "controller-secret";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    let listenerHoldsClaim = true;
    const secondReady = Promise.withResolvers<void>();
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState("omp"));
      if (url.pathname === "/legion/v1/controller/ready") {
        if (requests.filter((request) => request.path === url.pathname).length === 2) {
          secondReady.resolve();
        }
        return Response.json({});
      }
      if (url.pathname === `/v1/roles/${token}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
        }
        return Response.json({ role: token, holder: "ses_controller", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") listenerHoldsClaim = true;
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("controller handlers were not registered");
    const intervals: (() => void)[] = [];
    await sessionStart(
      {},
      { ...sessionContext("ses_controller"), setInterval: (callback) => intervals.push(callback) }
    );
    const ready = {
      path: "/legion/v1/controller/ready",
      body: { secret: "controller-secret", sessionId: "ses_controller" },
    };
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);

    // A steady tick: the listener still names this session, so nothing is claimed or re-run.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);
    expect(requests.filter((request) => request.path === "/v1/roles/set")).toHaveLength(1);

    // The listener lost the claim (tonight's incident): the next tick reclaims and drains.
    listenerHoldsClaim = false;
    intervals[0]?.();
    await secondReady.promise;
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready, ready]);
    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_controller", role: token },
      { session_id: "ses_controller", role: token, soft: true },
    ]);
  });
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `bun test extensions/legion.test.ts -t "regains its role re-runs controller/ready"`
Expected: FAIL — only one `/legion/v1/controller/ready` request (the `secondReady` await times
out, or the final `toEqual([ready, ready])` fails).

- [ ] **Step 3: Implement**

`legion.ts:32`:

```ts
import { claimEnvoyRole, onEnvoyRoleRegained } from "./envoy";
```

`legion.ts:199-200` — add one line after `controllerCapability`:

```ts
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let controllerRoleToken: string | undefined;
```

`legion.ts:249-262` — `claimController` records the token it claimed:

```ts
  const claimController = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const daemon = createLegionDaemonClient(requiredEnvironment(process.env, "LEGION_DAEMON_URL"));
    const secret = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = secret;
    const { project } = await daemon.state();
    const token = controllerToken(project);
    await claimEnvoyRole(sessionID, token, "setInterval" in context ? context : undefined);
    await daemon.controllerReady({ secret, sessionId: sessionID });
    controllerSessionID = sessionID;
    controllerRoleToken = token;
  };
```

Insert after `bootstrapWorker` (`:502`), before `pi.on("session_start", …)`:

```ts
  // The Envoy heartbeat re-established this session as `role`'s live holder after the listener
  // had lost sight of it (`reassertRole` in envoy.ts). Whatever the daemon published to the role
  // meanwhile got a 404 "no holder", and recovery differs by kind:
  //  - controller: the daemon queued each notice in `controllerPendingNotices` and only
  //    `/controller/ready` drains them (and forces a resync) -- the same call the boot handshake
  //    and `/legion-claim-controller` make, so re-run it (index.ts `onControllerReady`).
  //  - root architect: the daemon's no-holder recovery (`onUndeliverable` -> `resumeWorker`) is
  //    a no-op for the root's claim (no worker locator to resume), so nothing replays the missed
  //    wake; `/process/ready` re-emits the overseer catch-up (`onTreeReady`), so re-run it. A
  //    stale generation 409s, which `callReadyWithRetry` propagates without retrying.
  //  - phase worker (sub-architect included): `resumeWorker` -> `spawnWorker` already prompts or
  //    queues a state-derived catch-up on the live worker's own socket, and `/worker/ready` is a
  //    no-op once the boot is confirmed. Nothing to do.
  // Never throws: a failed ready call is logged, and the next regain or boot retries it.
  onEnvoyRoleRegained(async (role, reason) => {
    try {
      if (
        controllerSessionID !== undefined &&
        controllerCapability !== undefined &&
        role === controllerRoleToken
      ) {
        await createLegionDaemonClient(
          requiredEnvironment(process.env, "LEGION_DAEMON_URL")
        ).controllerReady({ secret: controllerCapability, sessionId: controllerSessionID });
        console.error(`[legion] re-ran controller/ready after role ${role} was ${reason}`);
      }
    } catch (error) {
      console.error(
        `[legion] ready call after role ${role} was ${reason} failed; the next regain or boot retries it: ${messageFor(error)}`
      );
    }
  });
```

- [ ] **Step 4: Run the tests**

Run: `bun test extensions/legion.test.ts -t "controller"`
Expected: PASS (the new test and the three existing controller tests).

- [ ] **Step 5: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" describe -m "fix(pi-envoy): a controller that regains its Envoy role re-runs controller/ready

Drains controllerPendingNotices and forces a resync through the daemon's
existing ready path instead of waiting for a human /legion-claim-controller." && jj -R "$LEGION_WORKSPACE" new
```

---

## Task 4: `legion.ts` — a root architect re-runs `/process/ready`; a phase worker does nothing

**Files:**
- Modify: `packages/pi-envoy/extensions/legion.ts` — the `onEnvoyRoleRegained` listener from Task 3
- Test: `packages/pi-envoy/extensions/legion.test.ts` — `bootWorker` (`:313-370`) gains an
  `intervals` option; two new tests after Task 3's

**Interfaces:** consumes Task 3's listener; `callReadyWithRetry` (`legion.ts:84-107`),
`roleDaemon()` (`:220-247`), `generation(process.env)`.

- [ ] **Step 1: Extend `bootWorker` to capture heartbeat callbacks**

In `bootWorker`'s options type add:

```ts
  readonly intervals?: (() => void)[];
```

and replace the context construction (`:367`) with:

```ts
  const context: SessionContext = {
    ...sessionContext(sessionId),
    cwd: options.workspace,
    setInterval: (callback) => {
      options.intervals?.push(callback);
    },
  };
```

- [ ] **Step 2: Write the failing tests**

```ts
  test("a root architect that regains its role re-runs process/ready so the overseer catch-up replays", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-root-regain";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    let listenerHoldsClaim = true;
    const secondReady = Promise.withResolvers<void>();
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname === "/legion/v1/process/ready") {
        if (requests.filter((request) => request.path === url.pathname).length === 2) {
          secondReady.resolve();
        }
        return Response.json({});
      }
      if (url.pathname === `/v1/roles/${token}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
        }
        return Response.json({ role: token, holder: "ses_root", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") listenerHoldsClaim = true;
      return Response.json({
        session_id: "ses_root",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");
    const intervals: (() => void)[] = [];
    await sessionStart(
      {},
      { ...sessionContext("ses_root"), setInterval: (callback) => intervals.push(callback) }
    );
    const ready = {
      path: "/legion/v1/process/ready",
      body: { tree, sessionId: "ses_root", secret: "root-secret", generation: 3 },
    };
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await secondReady.promise;

    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready, ready]);
    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_root", role: token },
      { session_id: "ses_root", role: token, soft: true },
    ]);
  });

  test("a phase worker that regains its role re-runs nothing: the daemon's own no-holder recovery prompts its catch-up", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const token = roleToken("omp", "REPO-43", "implementer");
    const intervals: (() => void)[] = [];
    let listenerHoldsClaim = true;
    const reasserted = Promise.withResolvers<void>();
    await bootWorker({
      role: "implementer",
      sessionId: "ses_worker_regain",
      workspace,
      requests,
      intervals,
      extraRoutes: (url, body) => {
        if (url.pathname === `/v1/roles/${token}`) {
          if (!listenerHoldsClaim) {
            return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
          }
          return Response.json({ role: token, holder: "ses_worker_regain", last_seen: 1 });
        }
        if (url.pathname === "/v1/roles/set" && (body as { soft?: boolean }).soft === true) {
          listenerHoldsClaim = true;
          reasserted.resolve();
        }
        return undefined;
      },
    });
    expect(requests.filter((request) => request.path === "/legion/v1/worker/ready")).toHaveLength(1);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await reasserted.promise;
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_worker_regain", role: token },
      { session_id: "ses_worker_regain", role: token, soft: true },
    ]);
    // The daemon's resumeWorker -> spawnWorker path (processes.ts) already prompts or queues the
    // worker's catch-up on a no-holder 404, and /worker/ready is a no-op on a confirmed claim.
    expect(requests.filter((request) => request.path === "/legion/v1/worker/ready")).toHaveLength(1);
  });
```

- [ ] **Step 3: Run them and confirm the root test fails, the worker test passes**

Run: `bun test extensions/legion.test.ts -t "regains its role"`
Expected: root test FAIL (one `/process/ready`), worker test PASS (it pins the no-op; it must stay
green after Step 4).

- [ ] **Step 4: Implement**

In the Task 3 listener, add the root-architect branch after the controller `if` block (inside the
`try`):

```ts
      if (capability?.kind === "root-architect" && role === capability.roleToken) {
        const root = capability;
        await callReadyWithRetry("root process/ready after role regain", () =>
          roleDaemon().processReady({
            tree: root.tree,
            sessionId: root.sessionID,
            secret: root.secret,
            generation: generation(process.env),
          })
        );
        console.error(`[legion] re-ran process/ready after role ${role} was ${reason}`);
      }
```

(`roleDaemon()`'s recovery swaps in a reissued secret on an `Invalid session secret` 401/403, the
same way the `legion` tool's proxied calls recover.)

- [ ] **Step 5: Run the whole legion test file**

Run: `bun test extensions/legion.test.ts`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" describe -m "fix(pi-envoy): a root architect that regains its Envoy role re-runs process/ready

The daemon's onUndeliverable recovery is a no-op for the root's claim, so the
repeat ready call is what re-emits the overseer catch-up. Phase workers need
nothing: resumeWorker already prompts their catch-up." && jj -R "$LEGION_WORKSPACE" new
```

---

## Task 5: Docs

**Files:**
- Modify: `packages/pi-envoy/AGENTS.md:60` (Critical conventions), `skills/legion-controller/SKILL.md:29-32`
- Verify only: `packages/envoy/AGENTS.md:53`

- [ ] **Step 1: `packages/pi-envoy/AGENTS.md`** — insert after the bullet ending
  "use the shared `envoy_role_get` transport operation for current role holders." (`:60`):

```markdown
- The registration heartbeat (`ensureHeartbeat`, `ENVOY_HEARTBEAT_MS`, default 120 s) re-asserts the session's held role after every successful re-registration: it reads `GET /v1/roles/<role>` and issues a soft `POST /v1/roles/set` only when the listener does not name this session as the live holder — a healthy tick writes nothing and appends no `envoy-role-claim` transcript entry. A 409 (a different live holder) drops the local claim, warns once, and ends re-assertion for that role; the newer holder is correct. A regain — or the first healthy tick after a registry outage, when a surviving claim may still have been unresolvable — fires `onEnvoyRoleRegained` (the `LEGION_ROLE_CLAIM_BRIDGE` slot `legion.ts` registers), which re-runs the controller's `/controller/ready` and a root architect's `/process/ready`; a phase worker needs nothing, the daemon's own no-holder recovery prompts its catch-up.
```

- [ ] **Step 2: `skills/legion-controller/SKILL.md`** — append one sentence to the paragraph
  ending "Never pass a secret as a command argument or copy it into a transcript." (`:29-32`):

```markdown
The claim is kept alive automatically afterwards: the Envoy registration heartbeat re-asserts it
and re-posts readiness whenever the listener loses sight of this session, so `/legion-claim-controller`
is the manual override, not a routine step after a listener restart.
```

- [ ] **Step 3: Verify `packages/envoy/AGENTS.md:53`** already reads "Role ownership is durable in
  the `envoy_roles` JetStream KV bucket. … listener restart restores the claim from that record …".
  No edit.

- [ ] **Step 4: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" describe -m "docs(pi-envoy): heartbeat role re-assertion and the 409 rule" && jj -R "$LEGION_WORKSPACE" new
```

---

## Task 6: Gates, push, PR

- [ ] **Step 1: Gates (once, from `packages/pi-envoy`)**

```bash
cd -- "$LEGION_WORKSPACE/packages/pi-envoy" && bunx biome check extensions/ src/ && bunx tsc --noEmit && bun test
```

Expected: all three clean. Fix formatting by running `bunx biome check --write extensions/ src/`
and folding the result into the relevant commit (`jj squash` into that change), never a separate
"format" commit.

- [ ] **Step 2: Handoff, bookmark, push, PR** — per the legion-worker skill: write
  `.legion/implement.json` (fields: `commits`, `filesChanged`, `deviationsFromPlan`, `testerNotes`
  naming Part C below and the exact `bun test` command), `jj split -m "implement: record handoff"
  .legion/implement.json`, then:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 10)' && \
  jj -R "$LEGION_WORKSPACE" bookmark set legion/LEGION-29 -r @- && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/LEGION-29
legion gh -- pr create --repo sjawhar/legion --base main --head legion/LEGION-29 \
  --title "fix(pi-envoy): sessions re-assert a lost Envoy role from the heartbeat; Legion roles re-run their ready call" \
  --body-file /tmp/legion29-pr-body.md
```

PR body: the READY format from the legion-worker skill, first line `Dispatch: LEGION-29`, the
`E2E` line left for the tester, `Fast-follow: none`, `Chain: not stacked`.

---

## Part C — Tester runbook

Both smokes run on this host against the production listener at `http://127.0.0.1:9020`. If a
`curl` to `/v1/*` answers 401, the deployment has `ENVOY_API_TOKEN` set: add
`-H "Authorization: Bearer $ENVOY_TOKEN"` to every curl. Never print a token.

### Acceptance 1 (and live evidence for 2 and 3): a real session re-holds `smoke-legion-29`

The changed extension must be the one that loads. Sessions do **not** load a repo checkout's
`omp.extensions` manifest (`docs/solutions/envoy/omp-extension-mcp-mounting.md`); they load the
installed plugin (`~/.omp/plugins/…` 1.4.0 or the `legion` profile's 1.3.0). Load the branch source
explicitly and exclude the installed one:

```bash
export WS="$LEGION_WORKSPACE"   # the branch checkout at the implementer's head
workdir="$(mktemp -d /tmp/legion29-smoke.XXXXXX)"
tmux new-session -d -s legion29-smoke -x 220 -y 50 -c "$workdir" \
  "env -u OMP_PROFILE ENVOY_HEARTBEAT_MS=10000 omp --no-extensions -e $WS/packages/pi-envoy/extensions/envoy.ts"
```

`omp` is the dotfiles shim (injects provider keys for the default profile; `OMP_PROFILE` must be
unset or a scratch profile gets no keys). `--no-extensions -e <path>` is the flag pair
`docs/solutions/envoy/omp-extension-mcp-mounting.md:109` records; confirm with `omp --help` if it
errors. Fallback if `-e` will not take a `.ts` file: `cd $WS/packages/pi-envoy && bun run build &&
./scripts/prepack.sh`, stage `dist/` + `package.json` at
`<scratch>/omp/plugins/node_modules/@sjawhar/pi-legion-envoy/`, and launch with
`XDG_DATA_HOME=<scratch>` (the LEGION-6 rig did this) — still with `OMP_PROFILE` unset.

**Before anything else, prove which extension loaded**: the extension logs
`extension instance loaded` with `extension: file:///…` at debug level; find the OMP log for the new
session under `~/.omp/logs/` and confirm the path is `$WS/packages/pi-envoy/extensions/envoy.ts`
(or the staged `dist/envoy.js`), not `~/.omp/plugins/…`. Record the line in the E2E evidence.

1. Wait for the TUI (`tmux capture-pane -p -t legion29-smoke` shows the status line with the cwd;
   `packages/pi-envoy/scripts/smoke-delivery.sh:110-127` has the wait/send helpers to copy).
2. Prompt: `Call envoy_whoami and reply with the session_id only.` → `SID=<that id>`. Cross-check:
   `curl -s "http://127.0.0.1:9020/v1/sessions?dir=$workdir" | jq -r '.[].session_id'`.
3. Prompt: `Call the envoy_role_set tool with role="smoke-legion-29" and reply DONE.` Then
   `curl -s http://127.0.0.1:9020/v1/roles/smoke-legion-29` → 200, `.holder == $SID`. Record
   `date -u`.
4. **Remove the claim out from under it** (equivalent to `nats kv del envoy_roles smoke-legion-29`,
   which cannot run here — NATS is off-host):
   ```bash
   curl -s -X POST http://127.0.0.1:9020/v1/interests/unsubscribe -H 'Content-Type: application/json' \
     -d "{\"session_id\":\"$SID\",\"topics\":[\"notifications.role.smoke-legion-29\"]}"
   curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9020/v1/roles/smoke-legion-29   # expect 404
   t0=$SECONDS
   ```
   The extension's own state is untouched (it still believes it holds the role), exactly the
   incident shape. Spec alternative: a second session's hard claim followed by its
   `envoy_unsubscribe` — see step 6 for the curl version of a rival claim.
5. Poll: `until [ "$(curl -s http://127.0.0.1:9020/v1/roles/smoke-legion-29 | jq -r .holder)" = "$SID" ]; do sleep 1; done; echo "re-held after $((SECONDS-t0))s"`.
   **PASS: re-held within 30 s** (one 10 s heartbeat plus jitter). The session's transcript (the
   newest `*.jsonl` under `~/.omp/agent/sessions/` written since `$workdir` was created) gains **no**
   new `envoy-role-claim` entry: `grep -c envoy-role-claim <file>` is unchanged from step 3 —
   Acceptance 2's transcript rule on the live surface.
6. **Negative control / Acceptance 3 live** — a rival live session takes the role:
   ```bash
   RIVAL=smoke-legion-29-rival
   curl -s -X POST http://127.0.0.1:9020/v1/interests/subscribe -H 'Content-Type: application/json' \
     -d "{\"session_id\":\"$RIVAL\",\"dir\":\"/tmp\",\"topics\":[\"notifications.agent.$RIVAL\"],\"port\":0,\"title\":\"rival\",\"driving\":false,\"self_subscribed\":true}"
   curl -s -X POST http://127.0.0.1:9020/v1/roles/set -H 'Content-Type: application/json' \
     -d "{\"session_id\":\"$RIVAL\",\"role\":\"smoke-legion-29\"}"
   sleep 25   # ≥ 2 heartbeats
   curl -s http://127.0.0.1:9020/v1/roles/smoke-legion-29 | jq -r .holder   # still $RIVAL
   tmux capture-pane -p -t legion29-smoke | grep -c 'is now held by session smoke-legion-29-rival'   # exactly 1
   ```
   Then release the rival's claim and show the first session does **not** take it back (re-assertion
   ended): `curl -s -X POST …/v1/interests/unsubscribe -d "{\"session_id\":\"$RIVAL\",\"topics\":[\"notifications.role.smoke-legion-29\"]}"`;
   over the next 25 s `GET /v1/roles/smoke-legion-29` stays 404. Clean up:
   `curl -s -X DELETE http://127.0.0.1:9020/v1/sessions/$RIVAL`.
7. Teardown: `tmux kill-session -t legion29-smoke`; `GET /v1/roles/smoke-legion-29` → 404 within
   15 s (the session deregisters on shutdown); `rm -rf "$workdir"`.

### Acceptance 6: listener restart on the LEGION deployment is a pause

Precondition (spec Errors row): the deployed image contains `RoleBucket = "envoy_roles"`.

```bash
docker inspect envoy-listener --format '{{.Config.Image}} {{.Image}} started={{.State.StartedAt}} restarts={{.RestartCount}}'
# expect ghcr.io/sjawhar/legion/envoy:fcd13a145853be600b4770d2d908dfc51396d767 sha256:e99c5079… (or newer)
entry="$(docker inspect envoy-listener --format '{{index .Config.Entrypoint 0}}')"
docker exec envoy-listener sh -c "grep -c -a envoy_roles $entry"      # ≥ 1 → precondition met
docker exec envoy-listener sh -c "grep -c -a ReleaseExpiredRoleClaim $entry"   # 0 on fcd13a14: #958 not deployed; record it
```

If the count for `envoy_roles` is 0, stop: report the tag/commit and let the architect open the
rollout `dispatch_ask` (spec Errors). Record tag, digest, and both counts in the E2E line.

1. Baseline:
   ```bash
   project="$(curl -s "$LEGION_DAEMON_URL/legion/v1/state" | jq -r .project)"
   CTL="legion-${project}-controller"
   curl -s "http://127.0.0.1:9020/v1/roles/$CTL" | jq .          # holder = controller session id → $CTL_SID
   curl -s "$LEGION_DAEMON_URL/legion/v1/state" | jq '{controllerPendingNotices, controllerLocator}'
   legion state | jq .roles > /tmp/legion29-roles-before.json
   date -u
   ```
2. Restart: `docker restart envoy-listener`; then
   `docker inspect envoy-listener --format '{{.State.StartedAt}} {{.RestartCount}}'` and
   `until curl -sf http://127.0.0.1:9020/healthz >/dev/null; do sleep 1; done; echo "listener up after $((SECONDS-t0))s"`.
3. Role: `until [ "$(curl -s http://127.0.0.1:9020/v1/roles/$CTL | jq -r .holder)" = "$CTL_SID" ]; do sleep 5; done; echo "controller resolved after $((SECONDS-t0))s"` — **PASS: ≤ 120 s** (the
   deployment's default heartbeat). A quick restart keeps every `envoy_sessions` entry (5-min
   TTL), so this normally resolves as soon as the listener is up.
4. Notices: `curl -s "$LEGION_DAEMON_URL/legion/v1/state" | jq .controllerPendingNotices` → **0**.
   Also record whether the controller re-ran readiness: the controller pane
   (`tmux -L legion-$project list-windows -t legion-$project`, then
   `tmux -L legion-$project capture-pane -p -t <controller window> -S -200`) or its OMP log shows
   `[legion] re-ran controller/ready after role legion-<project>-controller was reregistered`.
   That line appears only if a heartbeat tick fell inside the restart window; if none did, no
   notice can have been queued either and 0 with no line is also PASS — report which.
5. Roles unchanged: `legion state | jq .roles | diff /tmp/legion29-roles-before.json -` → empty.
6. Daemon health: `legion state` keeps answering (a publish that failed with a connection error
   during the restart goes fatal on the durable lane and the supervisor restarts the daemon — if
   `legion state` briefly refuses, wait and retry, and record the daemon restart).

Optional stronger check (requires the architect's explicit go-ahead — it is a deliberate ~2.5 min
production listener outage): `docker stop envoy-listener; sleep 130; docker start envoy-listener`
makes at least one controller heartbeat fail, so step 4's log line **must** appear within one
heartbeat of the listener returning and `controllerPendingNotices` must read 0 afterwards.

### Unit gates the tester re-runs

`cd $WS/packages/pi-envoy && bun test extensions/envoy.test.ts extensions/legion.test.ts` and
`cd $WS/packages/envoy && go test ./cmd/listener/ ./internal/store/` (Acceptance 7: unchanged
listener tests `TestRoleClaimRestoresWhileHolderIsLiveAndDropsAfterTTL`,
`TestSetRole_PersistsClaimMetadata` pass).

---

## Self-review against the spec

| spec item | plan |
| :--- | :--- |
| Acceptance 1 (re-assert within one interval; soft, own id, no `previous_session_id`) | Task 1 test 1 + Part C A1 steps 4–5 |
| Acceptance 2 (quiet steady state: `appendEntry` count, `/v1/roles/set` bodies) | Task 1 test 2 + Part C A1 step 5 transcript count |
| Acceptance 3 (409 ends re-assertion, one warning; listener semantics unchanged) | Task 1 test 3 + Part C A1 step 6; no Go change |
| Acceptance 4 (controller regain → second `controller/ready` with the secret) | Task 3 |
| Acceptance 5 (daemon finding with line references; other kinds re-run ready) | Part B; Task 4 (root re-runs `/process/ready`; worker pinned no-op) |
| Acceptance 6 (restart smoke, precondition) | Part A precondition satisfied; Part C A6 |
| Acceptance 7 (Go tests unchanged) | Part C unit gates |
| Acceptance 8 (docs) | Task 5 |
| Requirement "plan names which path dropped tonight's claims" | Part A: (b) via the deployed image's `Reap()`→`Remove()`→`releaseAllRoleClaims`, not (a), (c) unsupported |
| Errors: 409 / unreachable / GET 5xx / hook failure / image lacks `envoy_roles` / dead-session restore | R2 + Task 1; heartbeat catch; GET non-404 propagates to the same catch; Task 3 `try/catch`; Part C precondition; listener unchanged |
| Rejected: daemon re-claims, daemon polls, `previous_session_id`, hard claim per tick, role topic in subscribe, listener change, `minPluginVersion` bump | none of them appear |
