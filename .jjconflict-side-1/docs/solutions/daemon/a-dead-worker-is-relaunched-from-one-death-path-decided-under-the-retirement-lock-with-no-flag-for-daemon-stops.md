---
title: "A dead worker is relaunched from one death path, decided under the retirement lock in one write, with no flag for daemon-initiated stops — and a relaunch never waits on GitHub"
category: daemon
tags:
  - worker-death
  - relaunch
  - markWorkerDead
  - decideWorkerRelaunch
  - probe-worker
  - resync
  - mutateClaim
  - sameProcess
  - launchFailures
  - bystander
  - catch-up
  - FakeRuntime
date: 2026-09-15
status: active
module: packages/daemon/src/daemon/processes.ts (markWorkerDead, decideWorkerRelaunch, retireDeadWorkerLocatorLocked, probeWorkerClaim), packages/daemon/src/daemon/resync.ts (probeConfirmedWorkers), packages/daemon/src/daemon/catchup.ts (workerCatchup), packages/daemon/src/daemon/__tests__/fake-runtime.ts (FakeRuntime.crash)
related_issues:
  - "LEGION-179"
  - "sjawhar/legion#1122"
  - "LEGION-19"
  - "LEGION-26"
---

# A dead worker is relaunched from one death path, decided under the retirement lock in one write, with no flag for daemon-initiated stops — and a relaunch never waits on GitHub

## The problem

A phase worker whose process died mid-task — a tmux pane killed, a pod crashed — was noticed
and tidied up (locator cleared, session kept in `resumeSessionFile`) but never relaunched until
some later role-lane exception, durable 404, or `spawn_worker` happened to address its role.
The phase stalled silently. LEGION-26's kill-pod checkpoint found it while designing the
Kubernetes kill-pod-resume proof; the issue as filed said "nothing detects a dead worker", which
was not quite right and shaped the fix.

## Detection already existed; the relaunch decision did not

Read the observing paths before designing a detector. On both runtimes a killed process closes
its worker-shim stream at once: `onWorkerClientClosed` spends one reconnect, the runtime refuses
it, and the death is confirmed within seconds. A daemon restart's `reconnectWorker` finds the
same thing. Both ended in a retire-only `markWorkerDead`. So the fix is the *decision* after the
retirement, and the periodic resync probe (`probeConfirmedWorkers` → `{kind: "probe-worker",
token}` → `ProcessManager.probeWorkerClaim`) is only the backstop for the death a stream never
reports — a half-open connection, a claim this daemon holds no connection to. Waiting for a
ten-minute probe when the death is already known would have been a stall by design; probing only
would have missed nothing but cost the latency.

The backstop poller is deliberately dumb: it emits a probe for every worker claim with a
locator and `readyConfirmedAt`, whatever its tree's status. The "does this even matter" logic
(tree gone, closing, or lingering; token already queued; finished bystander) lives once, in the
terminal handler, so the three observations cannot disagree. `resync.ts` has no `closingTrees`
and no `rootForIssue`; giving it that judgement would have been a second copy.

## One death path behind three observations

`markWorkerDead(token, locator, observed)` is the single sink. `observed` is a
`WorkerDeathObservation` enum (`stream-closed` / `restart-reconnect` / `resync-probe`) that
parameterises only the log line's clause (`WORKER_DEATH_LABELS`); it branches nothing. When the
same terminal fact is reached by several detectors, resist giving each caller its own retirement
logic because its trigger differs.

Inside the role's `mutateClaim(token)` critical section, in order:

1. re-validate the locator with `sameProcess(current.locator, locator)` — a concurrent respawn
   can replace it mid-probe, and identity is pid + start ticks (tmux) or pod uid (kubernetes),
   never a pane id or name (a fresh private tmux server reissues `%1`; see the test note below);
2. evict and close a cached client for that locator — the resync path reaches a dead process
   whose stream never told us it closed, and without eviction the stop would `awaitShutdown` on
   it for the whole `worker_stop_timeout_seconds` under the lock; the evicted client's own close
   event then finds the cache moved on and returns;
3. `retireDeadWorkerLocatorLocked`: stop, clear the locator into `resumeSessionFile`, **return
   the claim without persisting**;
4. `decideWorkerRelaunch`; 5. one `persist()`.

The relaunch itself (`resumeWorker` → `deliverToWorker`, `--resume` from `resumeSessionFile`, a
`catchup` prompt, cap- and launch-hold-aware) runs **after** the lock releases: `deliverToWorker`
takes the same token's section itself and re-validates the claim under it (an architect's
`spawn_worker` landing in the gap wins over the catch-up). Re-acquiring a token's `mutateClaim`
from inside a callback that holds it awaits a promise that can only settle after that callback
returns — a permanent deadlock, the rule `stopProcessSerialized`'s comment already states.
Every new caller inside `mutateClaim` must audit its downstream calls for a second acquisition.

### Split the locked mutation from the commit

The old `markWorkerDeadLocked` retired *and* persisted. Two callers now need the same retirement
with different accompanying writes: the death path (locator clear + `launchFailures` + maybe a
queue push, in one durable transition — a crash between two saves would leave a locator-less,
uncounted, unqueued claim that `reconnectWorkers`' locator-only filter never sees again) and the
prompt-failure breaker (`retirePromptFailedClaim`, its own bookkeeping). So the retirement returns
the claim and the caller persists. The persisting wrapper the plan kept became a one-call-site
four-line function and was inlined into the breaker — the pattern is "extract the locked side
effect; let each caller commit", not "keep a wrapper for symmetry".

### The decision, and why no flag distinguishes a daemon-initiated stop

`decideWorkerRelaunch`, in this order, reusing the predicates that already exist rather than
re-deriving them (`isTreeGone`, `isBystanderRole`, `activePhaseLabel` in `legion-state.ts` —
grep for them before writing a new one over `LegionState`):

- tree gone, closing, or **lingering** → nothing (`closeTree` / the linger sweep own its
  workers; `isTreeGone` does not cover a durable `lingering` status, so that one is explicit);
- token already on `workerAdmission.queue` → nothing, one log line (the drain relaunches it cold,
  once — a second launch here would double it);
- finished bystander (`isBystanderRole` and no `pendingAssignment`) → retired only, one log line;
  only `spawn_worker` resumes a finished worker;
- else `launchFailures += 1`; at `MAX_LAUNCH_FAILURES` (`===`, exactly once per crossing, like
  every other threshold publish) `worker-died` to the owning architect and no relaunch;
- a claim still holding a pending prompt but no queue entry → `enqueueForRetryPending` so the
  drain relaunches it *with that prompt* (a catch-up would be dropped behind a queued assignment
  by `deliverToWorker`, and nothing else would deliver it);
- else return the retry context: the caller resumes the same agent with its catch-up.

Idle retirement, `closeTree`, and the breaker all clear the locator (or delete the claim, or sit
in `closingTrees`) **before** their stop returns; the stop's own socket close reaches
`onWorkerClientClosed` → `markWorkerDead` afterwards, whose `sameProcess` check fails on the
cleared locator (or `isTreeGone` answers for the close). No `daemonInitiated` flag, no state
field, no version bump — and the tests pin each of the five cases so a reordering that stops
before clearing would relaunch a worker the daemon itself retired.

The counter is `launchFailures`, reset only by a confirmed `/worker/ready`; a relaunch that never
confirms is the boot watchdog's and escalates on the same counter. A separate crash counter that
survives a healthy boot would be a new counter with a new reset rule for a case (a worker that
boots fine and dies every turn) that is an outage to wait out — roots already have this shape.

## A relaunch never depends on GitHub being reachable

`resumeWorker` computes `workerCatchup` before `deliverToWorker`, and that used to throw on the
App token mint or any `gh api` read. At the moment of a death the claim is already retired and
counted with its locator gone — invisible to the next resync probe — so a throw there stranded
the worker until the next unrelated wake. The GitHub reads are now one guarded enrichment
(`collectUnhandledFromGitHub`, pushing onto `unhandled` as it goes so a failure part-way keeps
what came before): a failure is logged once and named in the payload's `github.error` beside
whatever was gathered, and the catch-up still goes out; the resumed worker reads GitHub itself.
Known limit: the error names the pull request, not which of its four reads failed. Rule: a
recovery path's enrichment must never be its gate.

## Testing the death path

- `FakeRuntime.crash(locator, { closeSocket? })` is the failure vocabulary the path needed:
  `probe` answers `dead/gone`, `connect` refuses, and the handed-out client closes — unless
  `closeSocket: false`, the one flag that reaches the resync-only branch (with the socket closed
  the stream path relaunches first). Extend a fake with the exact failure the new path introduces
  and make the hard-to-reach state a flag, not a second fake.
- A FakeRuntime fixture must seed a **locator-less** active root tree, not the file's `tree()`
  helper: `closeTree` probes the root and `FakeRuntime` refuses tmux-shaped locators.
- The confirmation is seeded the way every worker fixture does (`sessionId`, the locator's
  `ompSessionFile`, `pendingAssignment` cleared, then the real `workerReady`) so the fake client
  is cached exactly as `/worker/ready` leaves it — the death path's eviction and identity checks
  run against real state, not a hand-built claim.
- Live tmux rows (`LEGION_TMUX_LIVE=1`): killing the private session's only pane tears the
  server down and the relaunch's fresh server may reissue the very same pane id, so assert
  `sameProcess(...)`, never `tmuxPaneId !==`. A hand-seeded confirmation leaves
  `launchFailures: 0` where `/worker/ready` deletes it; assert `toBe(0)`, not `toBeUndefined()`.
- `bun:test` clears `mock.calls` on `mockRestore()`: read the captured log lines inside the
  `try` before the `finally` restores the spy, or the assertion sees `[]`.

## What did not need to change

No `LegionState` version bump (every field the decision reads existed), no `LegionDaemonApi`
shape change (contract stays 5), no new architect wake (a promoted queued relaunch publishes the
drain's own `worker-started`, a direct relaunch publishes nothing — `worker-died` at the threshold
is the one wake the architect acts on), and no runtime branching: every liveness question is
`Runtime.probe`, every stop `stopProcess`.
