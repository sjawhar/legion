---
title: "Await the event, not a tick budget: the processes.test.ts observer fixture, next() before the trigger, and the inert-await trap"
category: testing
tags:
  - bun-test
  - flaky-tests
  - event-loop
  - fixtures
  - fake-timers
  - load-testing
  - processes.test.ts
  - worker-boot-watchdog
  - code-review
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/__tests__/processes.test.ts
related_issues:
  - "LEGION-15"
  - "LEGION-27"
  - "sjawhar/legion#1027"
  - "sjawhar/legion#981"
symptoms:
  - "a daemon test passes alone and fails under load with an assertion on state a background chain has not written yet"
  - "`flushEventLoopUntil(cond, 20_000)` returns silently and a downstream `expect` fails with `relaunched claim missing` or a stale `generation`"
  - "a converted `await counter.reached(n)` is green but resolves before the gated call it names has even run"
  - "`this test timed out after 5000ms` on a test that does two or three real relaunches under a busy box"
---

# Await the event, not a tick budget

## Context

`processes.test.ts` drives `ProcessManager` against fakes, but several chains under test do real
fs I/O between the fake calls: `spawnRoot`/`launchWorker` provision a workspace (`mkdir`), write a
0600 secret file, and `persist()` prunes with `rm`. Until sjawhar/legion#1027 the tests waited for
those chains by spinning `setImmediate` a fixed number of times — `flushEventLoopUntil(cond, 20_000)`
(about 23 ms of ticks) or `flushEventLoop(2_000)`. On a 32-core box whose 1-minute load sits at
100–150 the ticks run out before the writes land and the test fails although the daemon is right:
3/30 for `reconnectRoots re-arms…` at load ≈130 (LEGION-35), 1/60 for
`retires a started worker whose ready delivery cannot connect…` at load 145 (this issue's
tester), with the pre-fix assertion text `error: relaunched claim missing` — the 50,000-tick budget
gone in 661 ms while the relaunch's `mkdir` + `new-window` were still in flight.

#981 had converted one site by resolving a promise from inside the fake `run` (`windowCounter`).
#1027 generalised that into one fixture convention and converted all 53 sites: 33 positive waits
now await their event, 19 negative drains stay with a one-line reason, `flushEventLoopUntil` is
gone. Raising the tick budget was rejected in the spec: it moves the threshold, the race stays.

## The convention

Every injected dep `manager()` wires is observed from outside the test's own fake, so a test's
`run`/`saveState`/`sleep`/`natsPublish` override keeps working unchanged and the test awaits
through the observer:

| handle on `manager()` | counts | key |
| :-- | :-- | :-- |
| `saves: CallObserver` | every `deps.saveState` | — |
| `runs(verb): CallObserver` | every `deps.run` | `command[0] === "tmux" ? command[3] : command[0]` — `"list-panes"`, `"kill-pane"`, `"new-window"`, `"split-window"`, `"has-session"`, `"jj"` |
| `sleeps(ms): EventCounter` | every `deps.sleep`, **only when the test injected one** | duration |
| `published(type): EventCounter` | every `deps.natsPublish` | `JSON.parse(json).type`, `"?"` when absent |

`EventCounter` is `{count, increment(), reached(n), next()}`. `CallObserver` is
`{issued, completed}` — two `EventCounter`s, `issued` incremented before the injected fn runs and
`completed` after it resolves. `registrationDeadlineMs(config)` names the root/controller deadline
sleep; `waitFor(cond)` is a real 5 ms poll for the one effect with no seam (below).

**Which end.** Await `issued` when the fake is going to *block* on a gate the test releases later
(`await runs("list-panes").issued.reached(1); … probeGate.resolve()`) — awaiting `completed`
there deadlocks until bun's timeout. Await `completed` when the assertions need the call's result
to have reached the code under test. Say why at the call site: every `issued.reached` in the file
carries an `// Issuance, not completion: …` line.

**`completed` fires after the real I/O the fake performs.** The fixture's `run` wrapper does the
jj `mkdir` side effects on exit 0 and increments `completed` *after* them (before the pure stdout
defaulting), so an awaiter never resumes ahead of that `mkdir`. That ordering is load-bearing;
the wrapper's comment names it.

**`reached(n)` defers one macrotask after the count is met — unconditionally.** The natural
`if (count >= n) return Promise.resolve()` resumes the test before the code that incremented the
counter has run its own microtask continuation (the `freshClaim` write after `runtime.spawn`
resolves, the `stillUnconfirmed()` re-check after a probe). One `onceEventLoop()` after the
threshold covers every microtask-only continuation the event unblocked — and nothing more: any
further *real* I/O needs its own event. The three #981 sites were re-run to prove the deferral
inert for them. (Same hazard, different shape, as `socket-tests-observe-the-peer-not-the-clock.md`
§2's `.catch().finally()` hop.)

## `next()` before the trigger — the inert-await trap

`reached(n)` is only right when *n* is a fact about the test's story ("the 2nd `new-window`", "the
3rd probe"). When the prior count is incidental, use `next()` — `reached(count + 1)` captured
**before** the trigger:

```ts
const retired = worker.saves.completed.next();
expect(worker.clock.fire(600_000)).toBeTrue();
await retired;
```

Round-1 review of #1027 found the trap in its purest form. In `closeTree landing during the
pre-resurrect persist prevents the retry from resurrecting into a closed tree`, the converted
line read `await saves.completed.reached(2)` after `await processes.closeTree(root)` — but
`closeTreeLocked` persists unconditionally (three times), so `saves.completed.count` was already
**4** when the gate opened, `reached(2)` took its already-satisfied branch, and the released save
and its `treeStillUnconfirmed()` decline were never awaited. Green, CI-passing, and observing
nothing. The fix is the idiom above: capture `saves.completed.next()` after `closeTree` returns
and before `saveGate.resolve()`, then `await released`. Prove it the way the reviewer did — drop
`expect(saves.completed.count).toBe(1)` at the old await and read `Received: 4`; after the fix,
`toBe(4)` before the capture and `toBe(5)` after the await both pass.

Rule: a fixed `reached(n)` assumes a call graph nothing upstream perturbs. Any path that persists
on its own (closeTree, beginLinger, a promotion sweep) perturbs it. Capture `next()` at the
trigger unless *n* is the thing under test.

## `drainSpawns()` after a released save

The three pre-resurrect tests guard one regression: `treeStillUnconfirmed()` wrongly returning
true after `dispose()`, `confirmRootReady`, or `closeTree`, so `escalateOrRetryUnconfirmedRoot`
runs `onRetry()` → `resurrect` → `spawnRoot`. That resurrection does real `mkdir`/`rm`/
`writeSecretFile` I/O before any `new-window`, so `expect(windowCount).toBe(1)` one macrotask
after the released save cannot see it — the old 2000-tick drain very likely could. `resurrect`
records itself synchronously in `ProcessManager.resurrecting` and `drainSpawns()` awaits that map
and `spawns` in a loop until both are empty (#981's pairing for root spawns). So after the
released-save await: `await processes.drainSpawns();` — free when nothing started, and it
restores the negative assertion's teeth. A future change to `drainSpawns`/`resurrecting` has these
tests depending on it being exhaustive.

Prove a negative assertion still bites with a mutation: break the guarded check in
`processes.ts` (byte-restore afterwards), run the test, expect it to fail on `windowCount` — the
tester did exactly that at the round-2 head. See `mutation-proof-probe-tests.md` for the general
method.

## Negative waits stay drains, with a reason

A drain is right only when the fired deadline or clock reaches a synchronous decline — a
wait-identity check, a role check, `stillUnconfirmed()` with its entry cancelled, `disposed`, an
`architect` role — by microtask hops, with no file write or injected fake in flight. There is no
event to await because nothing happens. Nineteen sites stay `await flushEventLoop(…)`, each
directly under a one-line `// Negative wait: <what the fire reaches>` marker (≤100 columns; the
rule itself lives once in `flushEventLoop`'s doc), so `grep -n -B1 'flushEventLoop('` audits
the whole file in one read. One drain over nothing in flight at all (`armIdleRetire` no-ops at
`workerIdleRetireSeconds: 0`) was deleted, not commented.

Two assertions became tautologies once the wait was exact (`attemptsBeforeDispose > 0` right
after `connects.reached(10)`; `listPanesCalls >= 2` right after
`runs("list-panes").completed.reached(2)`) and were deleted — an exact await subsumes the
"at least n" check that a tick budget needed.

## Two clock pitfalls the conversion exposed

**Read the clock before the call that arms the timer you measure.** The boot watchdog is armed
as the last step of `launchWorker`, and its first poll `sleep(100)` fires inside `spawnWorker`'s
own tail. A `startTime` captured *after* `await spawnWorker(…)` is already 100 fake-ms into the
first interval, and the literal conversion of `never evicts a slow-but-alive boot…` failed its
first run with `Expected: >= 3000 Received: 2900` at the third probe. Capture `startTime` before
`spawnWorker`; the assertions stay.

**A test that does several real relaunches needs the explicit `}, 20_000)` its siblings carry.**
`…escalating to worker-died at the launch-failure threshold` awaits two real relaunches
(`provisionWorkspace` + `new-window`) and a publish; at bun's default 5 s it starved once in 60 at
1-minute load 148 (`this test timed out after 5000ms` at 6234 ms, median 452 ms otherwise) with
the awaited `published("worker-died")` still pending. Not the tick race — no budget remains to
exhaust — a fail-loud timeout on more real I/O than its siblings. The root-deadline tests doing
the same kind of I/O already carried the explicit timeout; the plan's instruction to keep those
applies to every test in that class.

## `waitFor` is the one exception, and it names its missing seam

`TmuxRuntime.preparePane` calls `writeSecretFile` from `secrets.ts` directly; `TmuxRuntimeDeps`
has no `writeSecret`, and the next injected call (`split-window`) is lane-serialised behind the
older generation by design. The generation-race test therefore polls the file's content on a real
5 ms timer with no tick cap — bounded only by bun's per-test timeout, so it fails loud, never
silently. Adding the seam would be a production change the issue's acceptance 5 forbade; the
architect ruled to keep the poll. The rule for polling as a last resort is already in
`wait-for-a-subprocess-file-by-polling-not-by-watching.md` and
`socket-tests-observe-the-peer-not-the-clock.md` §3; this is its third instance, and the doc
comment on `waitFor` names the seam so nobody reaches for it by default.

## Reproducing the race: the load recipe

The machine's own load (1-minute 80–110 on 32 cores) gave **0 failures in 247 pre-fix runs** —
60 each at the three named sites, 60 whole-file, 7 full-suite. The tester reproduced it only with
the plan's supervised busy-loop generator (`hub start legion-15-load`: 32 `while :; do :; done`
at nice 19) on top, **with the test loop itself at nice 19** so it competed with the spinners
directly: 1/60 at load 145. Record `/proc/loadavg` at the start and end of every loop and quote
it with the result; a 0/N under load 100 is a finding, not proof of absence — see
`widen-the-contender-count-before-calling-a-race-unreproducible.md` for the same lesson on the
instance lock. Post-fix, 23 converted tests × 30 isolated runs passed 690/690 at load 98–110,
and the tester's A/B under the generator passed 4354 60/60 at load 140–152.

Fixed-count regexes: bun's `-t` is a regex, so pick a plain substring without `( ) / ? [ %`, and
have the loop confirm every run's summary reports exactly the intended count (`3 pass` for an
`it.each`) — a filter that matches nothing is a silent 0/0 pass.

## Trip hazards in the fixture (know before you extend it)

- **Keyed observers are lazy.** `runs("list-pane")` or `sleeps(360000)` for a verb or duration
  that never fires creates a fresh, never-incremented counter; the only symptom is bun's timeout.
  Check the key at the wrapper's keying expression and the call site both.
- **`sleeps` observes nothing under real timers.** The wrapper is spread only when the test
  injected `sleep`; a `sleeps(…).reached(…)` in a test without an injected sleep hangs to the
  timeout. Deliberate and documented on the return type, but easy to trip.
- **`registrationDeadlineMs(config)` duplicates production's formula**
  (`workerBootTimeoutSeconds * 1000 * workerBootRegistrationDeadlineIntervals`, in
  `armRootRegistrationDeadline`/`armControllerRegistrationDeadline`). A change to the production
  arm math must update this helper in lockstep, or `sleeps(registrationDeadlineMs(cfg))` waits on
  a duration never armed.
- The verb keying (`command[0] === "tmux" ? command[3] : command[0]`) is inline in `manager()`;
  a third executable or a differently-shaped tmux argv needs that one expression updated.

## Related

- `race-regression-tests-that-fail-before-the-fix.md` — the earlier rounds on the same file
  that first said "await the persist, never a guessed number of ticks", and the hard-assert-the-
  precondition rule (`expect(readFileSync(architectFile)).toBe("boot-gen-2")` right after the
  wait) this conversion kept.
- `mutation-proof-probe-tests.md` — how to prove an assertion still bites by breaking the guarded
  production check.
- `socket-tests-observe-the-peer-not-the-clock.md` — microtask-hop ordering and the fake-timer
  seam; the same exception clause for polling.
- `wait-for-a-subprocess-file-by-polling-not-by-watching.md` — polling as a last resort.
- `widen-the-contender-count-before-calling-a-race-unreproducible.md` — the load recipe
  discipline for the instance lock.
- `../legion/review-thread-replies-fan-out-pr-review-wakes.md` — the review-round cost this PR's
  five-thread acceptance made visible.
