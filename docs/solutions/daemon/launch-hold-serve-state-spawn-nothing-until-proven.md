---
title: "The launch hold: serve state and the API, but open no pane until a boot dependency is proven — queue through the existing admission queue, replay held recoveries"
category: daemon
tags:
  - boot-ordering
  - launch-hold
  - admission
  - worker-admission
  - boot-probes
  - process-manager
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/index.ts, packages/daemon/src/daemon/processes.ts, packages/daemon/src/daemon/worker-admission.ts
related_issues:
  - "LEGION-28"
  - "sjawhar/legion#980"
---

# The Launch Hold

## Context

Before LEGION-28, `startDaemonLocked` awaited both OMP boot probes before loading state, opening
NATS, or binding the API: a probe retrying under load meant a daemon that answered nothing and
acked nothing for minutes, and a probe that failed meant a process that had done nothing yet.
The retry fix (see `boot-probe-kill-is-transient-not-a-verdict.md`) made the wait potentially
long, which made the blocking order untenable. The alternative — spawning panes before the OMP
is proven — would launch roots and workers against an OMP that might be the wrong binary.

The resolution is a single boot gate on *pane-opening*, not on serving.

## The pattern

### Start the dependency check first; await it only where it is needed

```
environment, runner, Dispatch token file
verifyLegionPluginContract()          ← synchronous: a manifest read, never load-sensitive
probes = (async () => { pi.agents; plugin-load })();  probes.catch(() => {})   ← STARTED, not awaited
tokenManager, state load, gate-off handling, NATS, ProcessManager, event pump, API bind,
worker stream, reconnectWorkers, pruneSecretFiles, pending-controller-notice drain/spawn
await probes                          ← THE HOLD: the last point before the first pane could open
enableLaunches() → reconnectRoots() → reconcileAdmission() → reconcileWorkerAdmission()
→ replayHeldRecoveries() → timers → signal handlers
```

The no-op `probes.catch(() => {})` exists only so a definitive negative that lands before the
hold is not an unhandled rejection; the real handling is at the hold: `await probes` in a
`try/catch` whose catch runs `stop()` (event pump, ProcessManager dispose, drain, save, worker
stream, API, NATS, instance lock — all idempotent) and rethrows, so `startDaemon` still rejects
and `legion start` still exits 1. `stop` and the timer variables it clears are hoisted above the
hold (`let lingerTimer: unknown` assigned later; `stop` guards `!== undefined`).

### One gate, every pane-opening path

`ProcessManager.launchesEnabled` (false from construction) and `WorkerAdmission
.workerPromotionEnabled`, both flipped by one `enableLaunches()`. While closed:

| path | behaviour while held |
|---|---|
| `admit(issue)` (root) | cap treated as full → `queued`, tree on `admission.queue` |
| `advancePromotionSweep()` | cap treated as full → sweep clears, nothing launches |
| `WorkerAdmission.launchOrQueue` / `resumeOrQueueExisting` | enqueue → `spawnWorker`/`resumeWorker` return `{status:"queued"}`, publish `worker-queued` |
| `resurrect(tree)` | recorded in `heldResurrects`, logged `resurrection of <tree> held until the OMP probe passes` |
| `ensureController()` | `heldControllerRequest = true`, logged `controller launch held …` |

Passing the probe runs the *same* drains the worker cap already uses (`reconcileAdmission`,
`reconcileWorkerAdmission`) plus `replayHeldRecoveries()` for the two held kinds. Nothing new
is invented for promotion: a spawn during the hold is exactly a spawn at the cap. A live idle
worker's resume (`resumeOrQueueExisting`) is held too — prompting a pane is a launch decision.

Paths that only stop or kill panes (`closeTree`, `beginLinger`, `stopProcess`) or that merely
connect to a pane that survived the restart (`workerReady`, `controlDirective`, `markTreeReady`)
are **not** gated: the hold is about starting work, not observing it.

Why flags and small held-request sets rather than a queue of deferred closures: every launch
site tests the same flag, so the invariant "nothing opens a pane while held" is checkable by
grep — `launchesEnabled` / `workerPromotionEnabled` at `admit`, `advancePromotionSweep`,
`launchOrQueue`, `resumeOrQueueExisting`, `resurrect`, `ensureController` — and
`replayHeldRecoveries` just drains two explicit sets. A captured continuation per deferred launch
would have to be constructed correctly at each site and could not be audited the same way.

### Replay respects state that changed during the hold — carefully

`replayHeldRecoveries` replays a held tree while it is `active` **or `dead`**, and drops it only
when it is neither. The first version dropped everything that was not `active`, which dropped
the one request the hold exists to defer: a root that survived the restart and exited during the
hold reports it (`POST /process/exit` → `markProcessDead` → `status = "dead"`), and `dead` is
precisely what a resurrection recovers from. Write the predicate against the statuses that
actually exist (`queued | active | lingering | dead | launch-failed | closed`) — "parked" is an
issue status, not a tree status.

### Abort the dependency check when boot fails for another reason

A pre-hold failure (state load, NATS, the API bind) rejects `startDaemon` while the probe chain
is still retrying or an attempt is still running. Without cancellation the chain keeps spawning
OMP in the background of a daemon that already gave up. `BootProbeOptions.signal` is checked
after every backoff (→ internal `ProbeAbortedError`), the backoff sleep is a cancellable timer
cleared on abort (`createCancellableSleep` — a raced-but-uncleared 5-minute `setTimeout` would
keep the process alive), and the same signal reaches every runner call so an in-flight OMP child
is killed. `startDaemon`'s outer catch and `stop()` both abort it.

Two facts about this design an operator or a future maintainer should know:

- The daemon's retry is unbounded by design. A launch that looks transient forever (a wrapper
  that always prints the marker and then segfaults) retries for the life of the process; the only
  operator-visible signal is the per-attempt log line with its growing delay. There is no
  ceiling, metric, or alert — a deliberate trade against the crash loop, recorded here so nobody
  adds a bound back without weighing that.
- Cancellation is a second lifecycle beside the retry loop's own control flow: `probes.catch(()
  => {})` swallows a pre-hold definitive failure, while the `AbortController` unwinds the backoff
  sleep and the in-flight runner call. A third exit path added to `retryBootProbe` later must be
  threaded through the same signal, or it will leak a wakeup or a child process.

## Consequences accepted

- `startDaemon` resolves only after the hold releases, so `legion start` writes its registry
  entry — and `legion status`/`legion stop <team>` first see the daemon — only once the probe
  has passed. The API port is known from config and answers throughout; the supervisor owns the
  process. The rejected alternative (resolve early, hold in the background) needs a second
  failure channel and a rewrite of every definitive-failure test; nothing required it.
- No SIGTERM handler is installed during the hold (they come after, as before). No panes exist
  yet to stop gracefully, and the durable lane saves before it acks.

## Testing the hold

Two layers, both worth keeping:

- Unit, against the closed gate (`processes.test.ts`, `skipEnableLaunches: true`): `admit` →
  `queued`; `spawnWorker` → `queued` + `worker-queued`; `resurrect`/`ensureController` held and
  logged; then `enableLaunches` + the reconciles/replay → one `new-window` each.
- Boot, with a real port (`index.test.ts`): a runner returning `timedOut` for the first two `sh`
  probes and a fake `sleep` that, on its **first** call, fetches `GET /legion/v1/state` (200,
  root queued, no `new-window` yet, `startDaemon` not resolved) then resolves. The injected
  `sleep` is shared with the ProcessManager's registration deadlines, so resolve only the probe's
  backoffs and leave later sleeps pending, or the deadlines fire mid-test and retire the panes
  you are asserting on.

The acceptance proof was a real `startDaemon` on this box under load average 85–110 with the OMP
launch wrapped to `exec sleep 60` for two attempts at `LEGION_SLOW_COMMAND_TIMEOUT_SECONDS=5`:
two `command timed out after 5 s` lines with 10 s / 20 s waits, `/legion/v1/state` 200 during the
hold with no tmux server yet, the root active with a pane after, and the negative control
(`LEGION_OMP_AGENTS=missing`) exiting 1 with the API refused afterwards.
