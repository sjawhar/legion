---
title: "A setImmediate tick budget over real filesystem I/O flakes under load: await the event the production path itself emits"
category: testing
tags:
  - bun-test
  - flake
  - flushEventLoopUntil
  - setImmediate
  - real-io
  - publishRole
  - event-wait
  - fake-client
  - stand-in-omp
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/__tests__
problem_type: test-stability
severity: medium
related_issues:
  - "LEGION-60"
  - "sjawhar/legion#1030"
symptoms:
  - "a processes.test.ts case passes solo and inside the full suite but fails 2 of 8 whole-file runs on a loaded box"
  - "the failure is always the same assertion, after the daemon has already logged the step under test (`respawning …`)"
  - "the wait before the assertion is `flushEventLoopUntil(() => <state>)`"
applies_when:
  - A daemon test asserts an end state that depends on a detached chain doing real filesystem or tmux work (a relaunch: session-file `stat`, secret file, workspace config, socket dir)
  - You are choosing how to wait for a detached chain in a unit test
  - You are widening a shared fake to model a new failure mode
---

# A tick budget over real I/O flakes under load

## The failure

Two regression tests (`retires a ready-time worker whose socket closes after acknowledging its first
task … (workerCap 1|2)`) waited for the cold relaunch with `flushEventLoopUntil(() => paneId ===
"%302")` — a budget of 5000 `setImmediate` ticks, each a few microseconds. The relaunch path does
real filesystem work: `stat` of the recorded session file, `mkdir`/`writeFile` of the boot-token
secret file and the workspace `.omp/config.yml`, `mkdir` of the socket dir. Completion of real I/O is
not tick-bounded. At loadavg ~110–130 the budget expired mid-relaunch in 2 of 8 whole-file runs,
always *after* the daemon had logged `respawning …` — the behaviour was correct, the wait was wrong.
One run also printed `recorded OMP session file is missing`: the test had already failed, `afterAll`
removed its temp dir, and the detached relaunch was still running against it.

## The rule

`flushEventLoopUntil` is for chains built entirely from injected fakes (a manual clock, an
instantly-resolving `sleep`), where every step is a microtask or a tick away. The moment the chain
does real I/O, wait on **the signal the production code emits at the end of the chain**, resolved
from inside the injected dependency that receives it:

```ts
const promoted = Promise.withResolvers<void>();
const { processes } = await workerCapFixture(cap, {
  publishRole: (subject, json) => {
    publications.push({ subject, json });
    if (subject === architectTopic && json.includes("worker-started")) promoted.resolve();
  },
});
await processes.workerReady(root, "planner", "ses_planner", 1);
await promoted.promise; // the relaunch published worker-started: claim written, dequeued, persisted
```

`worker-started` is published by `promoteQueuedWorker` only after `launchWorker` resolved (fresh
claim written, token dequeued, persisted), so every assertion behind it is deterministic, and a wait
that never resolves fails on bun's own test timeout with the assertions unreached — never a false
green. The same file already had this idiom (`windowCounter` for `new-window` launches; the
`publishRole` hook in the queued-promotion tests); reuse it rather than adding a wall-clock poll,
which is a third convention and still a guess.

After the change the file passed 10 of 10 whole-file runs at loadavg 97–101 and, after the
conflict-forced rebase, 10 of 10 more at loadavg 116–123 (the tester's failing range); the tester's
own round then ran it 20 of 20 at loadavg 130–139. The assertions were untouched.

## Two fake-client rules from the same tree

- **A widened shared fake defaults to today's behaviour.** `fakeWorkerRpcClient` grew
  `turnStartsOnPrompt`, default `true`, so the ~40 existing tests that assume a healthy worker
  (`agent_start` follows the acknowledgement) kept passing untouched; only a test modelling a
  swallowed prompt flips it. A default that models the *new* failure mode silently turns every
  existing case into a not-started path waiting on a real 5 s timer.
- **Mirror the real client's contract in both directions or say which half you mirror.** The fake's
  `getState` starts the pending receipt on `isStreaming: true` (as the real one does) but leaves the
  `false` half to the test's `getStateImpl` (`emitRunState("idle")`), which the fixtures already did.
  Its `close()` flips to `idle` and fires `onIdle`; the real client goes to `unknown` and never
  fires. Neither gap produced a false green (end-state assertions would catch a misordered drain),
  but each is a fidelity gap a reader must know about; the review recorded both as fast-follows.

## Reproduce before you pin

The round-1 blocking finding was pinned first as a disposable harness
(`/tmp/legion-60-repro/ready-close.test.ts`: fake client, turn never starts, socket closes right
after the ack, reconnect refused, run at two worker caps) and only then turned into the two
permanent tests. The harness proved the bug on the old code and, after the fix, failed on its own
"dead locator kept" expectation at both caps — the red/green the permanent tests then locked.

## When a fake is not enough

The three delivery outcomes were also driven end to end with a real tmux server and a real
`legion worker-shim` around a purpose-built stand-in for `omp --mode rpc`
(`cli/__tests__/fixtures/delayed-start-omp-rpc.ts`: negotiates protocol v2, acks `prompt` at once,
logs each prompt to `FIXTURE_PROMPT_LOG`, emits `agent_start` after `FIXTURE_AGENT_START_DELAY_MS`
or never when unset, answers `get_state` truthfully). The same fixture was the smoke rig's
negative control behind a real shim. A stand-in that speaks the real line protocol with
parameterised bad behaviour is built once and reused across the e2e and the rig; see
`docs/solutions/testing/driving-a-real-omp-worker-against-a-stand-in-daemon.md` for the mirror-image
technique.
