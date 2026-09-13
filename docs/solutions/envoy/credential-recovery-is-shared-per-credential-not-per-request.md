---
title: "A recover-then-retry wrapper that runs per request races itself when the recovery replaces the credential: share one in-flight recovery per credential, retry once with the newest, and build the client once per scope"
category: envoy
tags:
  - pi-envoy
  - daemon-client
  - session-secret
  - credential-recovery
  - concurrency
  - race-conditions
  - memoization
date: 2026-09-13
status: active
module: pi-envoy
problem_type: logic_error
severity: high
symptoms:
  - "Two legion tool calls in one batch: one returns `POST /legion/v1/<route> failed with 403: {\"error\":\"Invalid session secret\"}`, the other succeeds; the failed call succeeds when reissued alone"
  - "The daemon log shows two `/legion/v1/worker-session` recoveries for one session id within the same second"
  - "The failure appears only after a daemon restart (or any event that made the daemon forget the session's secret)"
root_cause: "The plugin's daemon client recovered a refused secret per request and roleDaemon() built a fresh client per call, so two requests refused together each minted their own replacement secret; the daemon keeps only the newest, and the request retrying with the older one was refused again with no further try"
resolution_type: code_fix
resolution: "sjawhar/legion#1032 (LEGION-73): per session id, the newest recovered secret plus the in-flight recovery are shared by every refused request; one retry per request; roleDaemon() is memoized to one client per session"
related_issues:
  - "LEGION-73"
  - "sjawhar/legion#1032"
  - "LEGION-27"
---

# A recover-then-retry wrapper that runs per request races itself when the recovery replaces the credential

## Context

The Legion daemon keeps each session's capability secret only in memory
(`CapabilityService.capabilities`, a `Map` in the daemon's `api/auth.ts`). After a daemon
restart every call from a live architect pane is refused once with `403 Invalid session secret`,
and the pi-envoy plugin recovers by POSTing `/legion/v1/worker-session` with the pane's boot
token, which mints a fresh secret and **overwrites** the stored one at mint time
(`setCapability` in `handleWorkerSession`). The daemon never honours two secrets for one
session, by design (a second live secret widens what a leaked one can do — spec Rejected).

On 2026-09-13 a root architect (LEGION-27) issued `escalate` and `spawn_worker` in one tool-call
batch right after a daemon restart. One was refused a second time; the same call succeeded when
reissued alone. The architect worked around it with strictly serial calls, which the architect
skill's decomposition step (`release_wave` then a `spawn_worker` per child) is designed not to
need.

## The race

The client's `post` wrapper did, per request: try → on `Invalid session secret` POST
`/worker-session` → retry once with the returned secret. Two requests refused together:

1. A and B both carry the forgotten secret; both are refused.
2. A's recovery mints `recovered-1`; B's recovery mints `recovered-2`. The daemon stores each at
   mint time, so `recovered-2` is the only valid secret before either response is read.
3. A retries with `recovered-1` → refused again → returned to the caller as the 403.

Nothing was wrong with the daemon; nothing was wrong with a *single* recovery. The defect is
that the recovery **replaces** the credential, so the second recovery invalidates the first's
result. Any client whose recovery is "mint a replacement" — a token refresh, a session re-key, a
lease renewal that rotates — has this race the moment two requests are refused together.

## The pattern (packages/pi-envoy/src/legion/daemon-client.ts, `secretAfterRefusal`)

Keep, **per credential identity** (here the session id), a record of the newest secret a completed
recovery returned and the recovery currently in flight. A request refused while carrying secret X:

| state of the record | what the refused request does |
| --- | --- |
| a newer secret than X is already known | retries with it, asks the daemon nothing |
| a recovery is in flight | awaits that same promise and retries with its result |
| neither | starts the one recovery, records it as in flight, retries with its result |

- **One retry per request.** A second refusal is returned to the caller: a genuinely revoked role
  must stay loud, and a daemon that restarted *again* between the recovery and the retry is the
  next request's problem, not a loop.
- **Validate before you cache.** `onRecovered` (the check that the recovered capability names
  this session's tree, issue, and role) runs inside the recovery's `.then` *before*
  `latestSecret` is assigned. A throw there rejects every waiter and records nothing, so the next
  refusal recovers afresh instead of retrying forever with a secret for the wrong role.
- **A settled recovery clears its in-flight slot** (success or failure) so the next refusal —
  including one carrying `latestSecret` itself, after a second restart — starts a new recovery
  rather than re-awaiting a settled promise.
- `Map<sessionId, record>` is right here: the key is a runtime value. With one client per
  session it holds one entry; nothing needs eviction.

## The record must live where every caller can see it

The algorithm alone did not fix the incident. `roleDaemon()` in `extensions/legion.ts` built a
new `LegionDaemonClient` on every call, and `createLegionTool` calls `roleDaemon()` per tool
execution — so two tool calls in one batch got two clients with two empty records and raced
exactly as before. The proof was mechanical: with the client fix committed and `roleDaemon()`
unchanged, the extension-level test still failed with `results.map(isError)` →
`[true, undefined]`. The fix is one client per session (`daemonClient ??= createLegionDaemonClient(...)`),
which every capability-bearing path shares: the `legion` tool, the bash grant hook, the
`/process/ready` re-run after an Envoy role regain, and the exit report.

Rule: when you add dedupe or cache state *inside* a client, check every construction site
in the same change. A per-call factory silently turns the shared record into a per-request one,
and every test that builds one client and issues two requests will pass while the product still
races. Memoizing captures `fetch` and `LEGION_DAEMON_URL` at first use; both are constant for a
pane's life, and every `legion.test.ts` case assigns its fake `fetch` before the extension boots.

## Testing it

`packages/pi-envoy/src/legion/daemon-client.test.ts` (`forgetfulDaemon`) and the
`extensions/legion.test.ts` case "two legion tool calls issued at once …". What made them fail
pre-fix for the defect's own reason rather than by luck:

- **The fake changes state when the real server does.** `/worker-session` mints and stores the
  new secret at *arrival*, and only the response is held on a gate the test owns. A fake that
  stored the secret when it *responded* would depend on how the two responses interleave. With
  mint-at-arrival, the pre-fix failure is the daemon's own message — `POST /legion/v1/waves/release
  failed with 403: {"error":"Invalid session secret"}` — whatever order the gates open in.
- **Gate on the daemon's observation, then one macrotask tick.** Await `firstRecoverySeen`,
  then `await new Promise(resolve => setImmediate(resolve))`: the fake is pure promises, so every
  microtask the two refusals queued has run and a second `/worker-session` request (the defect)
  is already recorded before the gates open. No wall-clock wait.
- **Assert the request sequence, not just a count.** `daemon.requests.map(r => r.secret)` →
  `["boot-secret", "boot-secret", undefined, "recovered-1", "recovered-1"]` pins which secret each
  retry carried; a count of recoveries alone cannot tell "shared the recovery" from "recovered
  twice but got lucky".
- **A fix in two layers needs a red at each layer.** The client tests went red → green with the
  algorithm; the extension test stayed red until `roleDaemon()` was memoized. Committing them as
  two commits with their own pre-fix quotes is what let the reviewer verify both halves were
  load-bearing. The PR body also carries the path-scoped re-check at the end (`jj restore
  --from main@origin` of only the two source files → 61 pass / 4 fail; restore → 65 / 0).

The end-to-end proof on a scratch daemon with a real architect pane — and why its control run on
the parent commit is what shows the batch actually raced — is in
[scratch-daemon-rig-proves-what-unit-tests-cannot](../testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md),
"Proving a plugin-side race through a real architect pane".

## Related

- `packages/pi-envoy/AGENTS.md`, Critical conventions — the one-sentence contract.
- [race-regression-tests-that-fail-before-the-fix](../testing/race-regression-tests-that-fail-before-the-fix.md)
  — the general discipline for gating a race and proving the pre-fix failure.
- [heartbeat-role-reassertion-and-regain-hooks](./heartbeat-role-reassertion-and-regain-hooks.md)
  — the other recovery path in this extension, and why its hook fires detached.
