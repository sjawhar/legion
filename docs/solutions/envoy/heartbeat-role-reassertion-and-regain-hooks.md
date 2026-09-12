---
title: "Heartbeat role re-assertion and regain hooks"
category: envoy
tags:
  - envoy
  - role-routing
  - heartbeat
  - session-registration
  - recovery-hook
  - omp-extension
  - task-subagent
  - legion-controller
  - regression-tests
date: 2026-09-12
status: active
module: pi-envoy
related_issues:
  - "LEGION-29"
  - "sjawhar/legion#970"
  - "sjawhar/legion#958"
symptoms:
  - "GET /v1/roles/<role> answers 404 for a session that is still heartbeating and believes it holds the role"
  - "controllerPendingNotices grows while the controller pane is alive; the daemon log repeats `Envoy publish to notifications.role.<project>-controller failed with status 404`"
  - "A controller that ran a `task` subagent never re-runs /controller/ready after regaining its role"
  - "After a listener restart, a Legion root architect misses its overseer catch-up until a human re-claims the role"
---

# Heartbeat Role Re-assertion and Regain Hooks

## Context

On 2026-09-12 the Envoy listener (`envoy-listener`, image `fcd13a14`) went through three
self-terminations between 05:23Z and 07:58Z. Afterwards the Legion controller and several root
architects held no Envoy role: `GET /v1/roles/legion-<project>-controller` answered 404 while
the controller pane was alive and re-registering every 120 s, the daemon's boot-time notice
drain looped on `status 404` (230 failures in its log by 14:49Z, 37 notices queued), and by
14:46Z 31 live sessions on the installed plugin held no role. A human re-ran
`/legion-claim-controller` twice and drained the queue by hand.

Three different mechanisms were involved, and conflating them was the first mistake the plan
avoided (`docs/plans/2026-09-12-heartbeat-role-reassertion.md`, Parts A–B):

| mechanism | owner | what it does | what it does not do |
| :--- | :--- | :--- | :--- |
| durable role ownership (`envoy_roles` KV) | listener | remembers who holds a role across listener restarts | nothing restores a claim once it is deleted |
| live session registration (`envoy_sessions`, 5-min TTL, refreshed by the 120 s heartbeat) | session (pi-envoy) | makes the holder resolvable; a lapsed entry makes `liveRoleHolder` answer "no holder" | does not re-assert the role; the old plugin never re-claimed after boot |
| catch-up of work published while the role was unresolvable | daemon | queues controller notices; recovers phase workers on a no-holder 404 | does not recover a controller or a root architect (see below) |

`sjawhar/legion#958` fixed the listener side: the old image's interest reaper
(`Reap()` → `Remove()` → `releaseAllRoleClaims` on any KV read error plus a 10-minute-stale
row) reaped 37 sessions between 05:23Z and 07:17Z, and #958 replaced it with a role-claim
reaper that only drops dead holders. That image rolled at 13:27Z and its first reaper cycle
(`reaped=11` at 13:32:31Z) removed exactly the eleven claims of dead LEGION-6/LEGION-9 sessions
and no live one. The rollout did not restore claims already lost: the controller's 404s
continued until the manual re-claim at 14:54Z. That gap — a live session that never re-asserts
— is what this change closes.

Two caveats the forensics could not remove. The old listener logs only a reaped *count*, so
which specific claims each cycle deleted is inferred from cardinality and heartbeat history,
not audited per key; a listener-side follow-on is to log role, holder, and path on every
claim deletion. And "persistence was not the gap" is true only in the narrow sense that
`envoy_roles` already existed — the listener's *deletion* lifecycle was wrong too, and both
sides needed fixing.

## The fix (session side only)

`packages/pi-envoy/extensions/envoy.ts` — after every successful heartbeat `registerSession()`,
`reassertRole` runs: `GET /v1/roles/<role>`; if the listener does not name this session, a soft
`POST /v1/roles/set` (no `previous_session_id`); the listener's atomic soft-claim rule is the
arbiter — a 409 (different live holder) drops the local claim, warns once, and ends
re-assertion, with no transcript entry so `omp --resume` can still arbitrate later. A landed
claim (`"reclaimed"`) or the first healthy tick after a *failed registration*
(`"reregistered"`) fires `onEnvoyRoleRegained(role, reason)` on the process-wide
`LEGION_ROLE_CLAIM_BRIDGE` slot.

`packages/pi-envoy/extensions/legion.ts` — the controller re-runs `POST /legion/v1/controller/ready`,
a root architect re-runs `POST /legion/v1/process/ready` with its generation (both through
`callReadyWithRetry`); a phase worker registers nothing.

## Learnings

### 1. Re-registration repairs the evidence a role lookup needs to detect the outage

After a listener outage longer than the `envoy_sessions` TTL, the durable claim is intact but
the listener answers "no holder" until the session re-registers. The re-registration itself
restores the holder, so a GET *after* it names the session and sees nothing wrong — yet every
publish during the window 404'd. The spec's "GET, then claim if not held" could never see this
hole; plan refinement R1 added `"reregistered"`: the first healthy tick after a *failed*
registration fires the hook even though no claim was sent. The tester proved it live at the
request level (round 3: registration failure → registration OK with a failing role read →
healthy ⇒ exactly one `controller/ready`, none earlier).

General form: reconciliation that repairs a dependency before inspecting the dependent state
must carry the transition ("we were unregistered") forward itself. A healthy post-repair
snapshot does not prove no work was missed.

### 2. "An error happened" and "recovery is owed" are different variables with different boundaries

The regain obligation is *set* only by a failed registration — a failed role read after a
successful registration is a plain heartbeat error and must not trigger a redundant
`/controller/ready` (drain + forced resync) or `/process/ready` (an overseer-catch-up steer into
the architect's turn). It is *cleared* only once the whole tick — registration **and**
re-assertion — succeeded, so a recovery tick whose role read errors still owes the regain to
the next healthy tick.

Round 1 caught the false-positive (`heartbeatOutageNotified` doubling as the flag: any tick
error fired `"reregistered"`). The fix for that introduced the false-negative round 2 caught:
clearing the flag on registration success, *before* the same tick's `reassertRole` ran, lost the
owed regain when that role read failed. The minimum discriminating sequence is three ticks
(fail → partial success → healthy), not fail → success. Final code: `envoy.ts` `ensureHeartbeat`
— the rejection branch is the sole setter; the full-chain-success `.then` is the sole clearer.

### 3. A keepalive must never own the latch that a downstream recovery call can hold

`ensureHeartbeat` runs one tick at a time under a `healing` latch. Round 1 found the regain
hook `await`ed inside that chain: the hook is a daemon round-trip this side cannot bound
(`daemon-client.ts` fetch has no timeout; `/controller/ready` awaits `drainControllerNotices`,
whose retry loop is unbounded in attempts). A stuck drain pinned `healing = true`,
`registerSession()` stopped, the registry entry lapsed, the listener dropped the claim — the
recovery hook produced the outage it was meant to end. The tester later held a fake daemon's
ready response for 25 s and watched registrations continue at 10 s and 20 s inside the hold.

Fix: fire the hook detached — `void Promise.resolve().then(() => regained(role, reason)).catch(logger.warn)`
— so the chain (and the latch) settles once the listener calls do. Retry-count bounds
(`callReadyWithRetry`) are not latency bounds; wrapping the call in retries alone would not
have protected the heartbeat. Same principle as
[refcounted-hold-guards-a-derived-prune](../daemon/refcounted-hold-guards-a-derived-prune.md):
release a guard at the state transition it protects, independently of later side effects.

### 4. One process does not mean one extension instance: bind a callback only once the identity exists

OMP re-binds every extension factory to a `task` subagent's own `ExtensionAPI` in the same
process (oh-my-pi `sdk.ts` `bindPreparedExtensions`). `legion.ts` registered the regain listener
at `legionExtension(pi)` setup on the bridge's single last-registration-wins slot, so the first
`task` the controller ran replaced its listener with a subagent instance's — one whose
`controllerSessionID`/`capability` were all `undefined`. A regain then soft-claimed fine and
never re-ran `/controller/ready`. The root architect was shielded only because its `task` tool
is gated; the controller has no such gate. Reviewer reproduction: boot the controller, run a
second `legionExtension(createPi().pi)` plus a subagent-transcript `session_start`, remove the
claim → `/controller/ready` count stays 1.

Fix: register from the two paths that establish an identity — `claimController` after
`controllerReady` succeeds, `bootstrapRoot` after its `process/ready` — never at setup. A
subagent instance never reaches either path. This repairs the *regained* slot only:
`bridge.claim` is still assigned at factory setup on `main`, so `/legion-claim-controller` and
the `reclaim-architect` directive resolve to a subagent's envoy instance after any `task`
call (review round 1 `preExistingForRetro`; follow-on).

Audit rule for any process-global slot in pi-envoy: enumerate every writer and ask which
extension *instance* runs it, not which module.

### 5. Map catch-up by role kind and delivery lane before replaying a ready call

A ready endpoint is not generically a replay endpoint. On a no-holder 404 the daemon's
`onUndeliverable` → `resumeWorker` → `spawnWorker` prompts or queues a state-derived catch-up on
a live phase worker's own socket, so a worker needs nothing on regain (`/worker/ready` is a
no-op once the boot is confirmed). For a root architect the same path no-ops — the root's
claim has no worker locator or resumable session file — and only `/process/ready`
(`onTreeReady` → overseer catch-up) replays the missed wake; the listener *exception* lane's
`reclaim-architect` directive is a separate recovery that never fires for the daemon's own HTTP
404. For the controller, every undeliverable notice lands in `controllerPendingNotices`;
`/controller/ready` drains it and forces a resync.

Qualification the retro's fresh-eyes reader caught: "only `/controller/ready` drains" is not
quite right. `index.ts` also starts a boot-time drain when the daemon boots with notices
pending and a controller claim, and that loop retries until success — but its no-holder path
calls `ensureController`, a no-op while the controller pane is alive with its daemon-side claim
set, so the loop spins on 404 (the 230 log lines) until the role is re-held, and nothing
re-runs the ready call for the missed wake itself. The hook closes the running-daemon case where
newly held work has no active drain and forces the resync either way.

### 6. GET is an optimisation, not the ownership decision; local surrender is not durable release

When the GET names a *different* holder the heartbeat still issues the soft claim and lets the
listener's atomic rule answer (409, or success if that holder died in between) — one arbiter,
one code path (plan R3; see
[unsubscribe-resurrection](unsubscribe-resurrection.md) for the same authoritative-write
principle). A 409 drops only `claimedRoleTopic` — no `envoy-role-claim` release entry — so a
later `omp --resume` soft-reclaims from the transcript and the listener arbitrates again (R2).
Recording a release would make that resume give up a role nobody holds.

### 7. Recovery hooks need a failure-category × lifecycle × completion-boundary matrix, not "an outage test"

Every blocker across three review rounds was a composition the single-scenario tests could not
see: an identity-less instance between claim and loss; a hook that never settles; a role read
failing on the recovery tick. The reviewer's six-ordering trace (repeated failure, partial
recovery, drift, landed soft claim, 409, never-settling hook) found them by enumeration. The
tests that now pin them, all in `packages/pi-envoy/extensions/{envoy,legion}.test.ts`:

- "a regain hook that never settles does not block the next heartbeat tick's registration"
- "a failed role read does not make the next healthy heartbeat report a regain"
- "a failed registration's regain survives a role-read failure on the recovery tick"
- "a controller's regain hook survives a task subagent's in-process extension re-bind"

Each was proven red on the pre-fix code by swapping the production file back (see
[race-regression-tests-that-fail-before-the-fix](../testing/race-regression-tests-that-fail-before-the-fix.md)).
Note that the awaited-hook and setup-time-registration shapes were in the *plan*; the design
review needed to inspect OMP's factory re-binding lifecycle and the daemon call's termination
properties, not just the listener contract.

### 8. Live smokes: inject faults that survive the transport's retry, and prove which extension loaded

`@legion/envoy-client` retries one 5xx after 250 ms, so a fault proxy must answer *two* 503s to
fail one client call — a single injected error disappears inside the retry. Sessions load the
*installed* plugin, not a checkout: launch with `--no-extensions -e <branch>/envoy.ts`, `OMP_PROFILE`
unset, and read `extension instance loaded extension=file:///…` from the OMP log before trusting
any result (this box had 1.5.0 on the default profile and 1.3.0 on the `legion` profile — the fix
reaches Legion panes only when the `legion` profile's pin moves). Keep the proof boundary honest:
the controller smokes here used a real OMP session and listener against a *fake* daemon; the
production drain-to-zero was observed after the manual re-claim, not after a staged restart.

## Limits

A regain re-runs the ready call once, with bounded retries; a definitive 4xx or an exhausted
budget is logged (`[legion] … after role <t> was <reason> failed; the daemon's held work stays
undelivered until the next regain or boot`) and nothing retries until the listener loses the
claim again. Detachment bounds the heartbeat, not the daemon request's own lifetime
(`daemon-client.ts` still has no fetch timeout). "Within one heartbeat" describes the
reconciliation attempt under healthy transport, not an end-to-end delivery guarantee.

## Related

- [core-nats-receipt-backed-current-holder-delivery](../architecture-patterns/core-nats-receipt-backed-current-holder-delivery.md) — a fresh holder GET is routing state, not proof a message was accepted.
- [nats-kv-dual-bucket-lifecycle](../architecture-patterns/nats-kv-dual-bucket-lifecycle.md) — the interest/registration split; predates role routing, so "first subscribe after restart re-establishes everything" does not cover a deleted role or Legion catch-up.
- [session-id-remint-stale-transcript-identity](session-id-remint-stale-transcript-identity.md) — session-id re-minting is a different axis from same-process factory re-binding.
- [worker-pane-shell-gotchas](../legion/worker-pane-shell-gotchas.md) — the pane-environment and grant-prelude problems hit while shipping this.
- `packages/pi-envoy/AGENTS.md` (heartbeat bullet) and `skills/legion-controller/SKILL.md` (claim kept alive automatically) carry the current-behaviour statements.
