---
title: "A killed boot probe is transient, not a verdict: classify runner kills before markers, carry limit and wall time, never hand a load failure to the supervisor loop"
category: daemon
tags:
  - boot-probes
  - command-runner
  - timeout
  - retry-policy
  - supervisor
  - host-load
date: 2026-09-13
status: active
module: packages/daemon/src/state/fetch.ts, packages/daemon/src/daemon/boot-probes.ts
related_issues:
  - "LEGION-28"
  - "sjawhar/legion#980"
symptoms:
  - "[legion] Configured OMP invocation does not expose pi.agents: LEGION_OMP_AGENTS=available (partial output) → exit 1 → supervisor relaunch every second"
  - "Command failed (exit 1): jj git fetch ... with empty stderr under IO pressure"
  - "a boot probe that printed its negative marker and then hung was retried instead of refused"
---

# A Killed Boot Probe Is Transient, Not a Verdict

## Context

Twice on 2026-09-12 a disk storm took the Legion daemon down. Its boot-time OMP capability probe
(`omp models --extension <probe.mjs>`, run through a shared command runner with a fixed 30 s
kill timer) could not finish; the runner killed it; the caller saw a non-zero exit with no
marker and classified it exactly as it classifies a wrong OMP binary: definitive, exit 1. The
supervisor's `restart=on-failure` relaunched the daemon every second, and every relaunch spawned
another OMP probe into the load the daemon was dying of. The same 30 s runner killed `jj git
clone`/`fetch` during workspace provisioning, reported as `Command failed (exit 1)` with empty
detail (LEGION-16/22/23 launch failures).

PR sjawhar/legion#980 fixed this. The reusable lesson is about how a caller must be able to
tell "we killed it" from "it answered", and what a boot check owes its supervisor.

## The pattern

### 1. The runner reports its own kill as a distinct outcome

`CommandResult.timedOut?: { limitMs, elapsedMs }` (`packages/daemon/src/state/fetch.ts`) is set
only when `defaultRunner`'s kill fired **on a still-running child** (`proc.exitCode === null &&
proc.signalCode === null` when the timer fires). `elapsedMs` is wall time from spawn to exit.
Two consequences a future caller must respect:

- A kill exits 143 (SIGTERM). Without `timedOut`, 143 is indistinguishable from the child
  dying for its own reasons — and a caller that reports `Command failed (exit 143)` with an
  empty stderr has thrown away the one fact the operator needs.
- The guard matters. Bun keeps the stdio pipes open while any grandchild holds them; a shell
  that exits at once but left `sleep 1 &` behind makes the runner drain output past the budget,
  and the kill timer fires on an already-exited child. Reporting that as a timeout sends a probe
  that actually finished back into a retry it never needed (`fetch.test.ts`: "does not report a
  timeout for a command that already exited when the timer fired", and the signal-terminated
  sibling — a child killed by a signal has `exitCode === null` and `signalCode` set).

The runner also takes `signal?: AbortSignal` and kills the child when it aborts: a caller that
gives up (a daemon whose boot failed elsewhere while a probe was still running) must not leave
the child behind, because the kill timer dies with the caller's process. The tester observed
`sleep 60` alive 45 s after the daemon had exited before this existed. That kill is reported as
`aborted: true`, never as `timedOut` — the first version reported both kills as a timeout, and
the daemon then logged `probe failed transiently … command timed out after 300 s (ran 1.0 s);
retrying in 10s` for an attempt it had abandoned and would never retry (LEGION-56, the
fast-follow named in #980's approval). A kill the caller asked for says nothing about the
command; at most one of the two markers is present, the first kill to land deciding which.

### 2. Callers classify the kill before any marker logic — but a printed negative wins

`boot-probes.ts`'s `killedOutcome(result, stderrTail, negativeMarker)` runs before the marker
checks: an `aborted` kill ends the chain silently (no transient line, no retry, no diagnosis —
the caller's own start-up error is what surfaces; `retryBootProbe` also ends silently when the
signal has aborted by the time an attempt returns, whatever it returned), and a budget kill with
no answer is transient. The one exception is a probe that printed its negative marker
(`LEGION_OMP_AGENTS=missing`, `LEGION_PLUGIN_LOADED=no`) and *then* hung past the budget: the
answer is in, and it is definitive. The reviewer caught the first version of this, which retried
a `missing` that happened to be followed by a hang. The rule:

- runner kill on the caller's abort → abandoned (silent end; the caller's error surfaces)
- budget kill, no marker → transient (retry)
- marker present + non-zero exit (OMP loaded the probe, then died under load) → transient
- negative marker, whatever the exit → definitive
- clean exit without the marker, or the launch command failing before OMP ran → definitive

Each probe's error factory receives a `reason` (`"definitive" | "exhausted"`) so an exhausted
bounded retry says `<probe> never completed within its retry budget (N attempts)` instead of
misreporting what it never got an answer to.

### 3. A daemon under a supervisor retries transient failures itself, unbounded

`DAEMON_PROBE_RETRY = { initialDelayMs: 10_000, maxDelayMs: 300_000 }` with no `maxAttempts`:
delay for the i-th failure is `min(10 s · 2^i, 5 min)`, each attempt logged with its delay
(`[legion] OMP pi.agents probe failed transiently (attempt N); retrying in <d>s: command timed
out after <limit> s (ran <wall> s)`). A bounded retry ends in exit 1 and hands the problem back
to the loop this fix exists to stop. `legion probe-image` keeps a bounded policy
(`IMAGE_PROBE_RETRY`, six attempts) because an image build has no supervisor and must finish.
`restart=on-failure` stays the right supervisor policy — the daemon simply no longer fails for a
transient reason.

### 4. Slow commands get a named budget, not the generic default

`slow_command_timeout_seconds` / `LEGION_SLOW_COMMAND_TIMEOUT_SECONDS` (default 300) budgets
both boot probes and every workspace-provisioning command — anything that waits on OMP start-up,
the network, or a credential helper. The runner's 30 s stays for GitHub API reads. `legion
probe-image` derives `IMAGE_PROBE_TIMEOUT_MS` from the exported config default so the image gate
and a default-configured daemon agree.

## What to reuse

- Any runner with a kill timer must surface the kill as data (`timedOut` with limit and wall
  time), guard it against an already-exited child, and take an abort signal whose kill it
  reports separately (`aborted`) — a caller's own abandonment must never read as the command
  running out of time; callers must never collapse either kill into `Command failed (exit N)`.
- A boot check spawning a subprocess has three non-answers, not one: abandoned by its caller
  (say nothing), killed by its budget (transient), answered negatively (definitive). Decide
  "did the caller give up?" then "did it answer?" before "what did it answer?", and let a
  printed negative override a later budget kill.
- Under a supervisor, a transient failure is the daemon's to wait out; only a definitive one
  exits. Log every attempt with its delay so the operator sees waiting, not a stall.
- A check that spawns nothing and cannot be load-sensitive (the plugin contract manifest read,
  `verifyLegionPluginContract`) stays a synchronous pre-state refusal; do not fold it into the
  retry chain just because it lives next to probes that need one.
