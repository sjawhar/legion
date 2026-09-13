---
title: "Recovery timers: a retry that throws after its deadline was cancelled must re-arm, and a watch that dropped its own entry before an await must re-check cancelled/disposed after it"
category: daemon
tags:
  - timers
  - registration-deadline
  - worker-boot-watchdog
  - cancellation
  - processes.ts
  - worker-boot-watchdog.ts
  - dispose
date: 2026-09-13
status: active
module: packages/daemon
problem_type: correctness
severity: medium
related_issues:
  - "LEGION-27"
  - "sjawhar/legion#981"
applies_when:
  - A deadline or watchdog cancels its own timer before running a recovery step that can throw
  - A loop removes its own tracking entry before awaiting an operation that may itself cancel the loop
  - `dispose()`/`cancelAll()` promises that no background timer outlives the owner
---

# Recovery timers: a retry that throws after its deadline was cancelled must re-arm, and a watch that dropped its own entry before an await must re-check cancelled/disposed after it

Two defects found in review, both introduced by making a probe able to throw where it never could
before (see `a-failed-list-panes-proves-nothing-about-the-pane.md`). They are the same lesson from
two sides: once a step inside a timer's expiry can fail, ask what is left armed afterwards.

## 1. The step after the cancel can throw -- so the cancel must not be the last word

`escalateOrRetryUnconfirmedRoot` runs when a root's registration deadline elapses on a dead pane: it
cancels that generation's deadline, counts a launch failure, persists, and awaits `resurrect`.
`resurrect` re-probes -- and since the probe can throw, `await onRetry()` can throw. The only
handler was the timer's own `.catch`, which logged. The tree was left active and unconfirmed with
its locator intact and **nothing armed**: `resync` probes only confirmed roots, so the root sat
until a daemon restart or an unrelated exception happened to touch it. The AGENTS.md sentence
"the registration deadlines log and retry" was simply untrue for this path.

Fix: wrap the retry where the deadline was cancelled, and re-arm the *same generation* while
`treeStillUnconfirmed` still holds:

```ts
try {
  await onRetry();
} catch (error) {
  if (this.treeStillUnconfirmed(this.deps.state.trees[treeKey], generation)) {
    console.error(`… locator is untouched and its registration deadline is re-armed:`, error);
    this.armRootRegistrationDeadline(treeKey, generation);
  } else {
    console.error(`… no longer that generation's active, unconfirmed root (${observed}) …`, error);
  }
}
```

The decline branch matters: a `spawnRoot` throw inside the same retry has already queued or
launch-failed the tree, and a newer generation may have taken it over. Log what was observed
(status, generation, confirmation) rather than enumerating reasons -- the enumeration was wrong
twice in review.

The regression test's shape: the deadline's probe finds the pane gone, the resurrection's re-probe
answers `exit 1: tmux: server not responding`, and the test awaits the *second* `sleep` call (the
re-armed deadline) rather than polling state. Before the fix it hangs there.

## 2. Removing your own entry before the await blinds you to a cancel during it

The boot watchdog's `retire()` deletes the armed entry *before* awaiting `retireUnconfirmedBoot`,
on purpose: the retirement's own `cancel(token)` must not flip `cancelled` on the very watch that
is retiring it. That also means a `cancelAll()` (daemon dispose) or a same-token cancel landing
during the await finds nothing to cancel. When the retirement then failed, the catch declined to
re-insert -- correctly -- but returned "not finished", and the loop ran another full interval:
real timers, connect dials, a `list-panes`, and a second retirement whose `persist()` landed after
the daemon's final save. `cancelAll()`'s contract ("no background timer may outlive the watchdog")
was broken by the very code added to re-arm on a failed stop.

Fix: after the await, re-check every signal the pre-delete could have hidden, and finish on any of
them:

```ts
} catch (error) {
  if (cancelled || this.disposed || this.armed.has(token)) {
    console.error(`… cancelled, disposed, or superseded meanwhile; not re-armed:`, error);
    return true; // finished
  }
  this.armed.set(token, { generation, cancel });
  return false; // one more interval
}
```

`cancelled` catches a cancel that found the entry before the delete; `disposed` catches
`cancelAll()` (it sets the flag whether or not an entry exists); `armed.has(token)` catches a newer
arm that took the token. A same-token cancel that found no entry sets nothing -- that watch runs
one more bounded interval, and `retireUnconfirmedBoot`'s own re-validation of the claim it is
handed makes the second call a no-op. Say exactly that in the doc; two review rounds went to
wording that named the wrong mechanism.

The regression test uses the file's `trackRealTimers()` and no fake `sleep`: the retirement blocks
on a gate, `cancelAll()` lands, the gate releases to throw, a real 250 ms passes -- no probe, no
connect, no second retirement, `activeCount() === 0`. With a fake instant `sleep` the timer
assertion is inert; with the guard removed the row sees eleven more events.

## Related

- `idle-expiry-timers-rearm-on-declines-that-change-while-idle.md` -- the idle-retire clock's
  version of "judge at expiry, re-arm on the declines that can change".
- `launch-hold-serve-state-spawn-nothing-until-proven.md` -- the other place `dispose()` and
  in-flight recoveries meet.
