---
title: "A derived wait gets a floor; the daemon refuses at load the configuration that cannot fit it; the downstream binary stays permissive for skew; the test proves the floor minimal"
category: daemon
tags:
  - stop-grace
  - terminationGracePeriodSeconds
  - worker_stop_timeout_seconds
  - config-refusal
  - floor
  - version-skew
  - fake-clock
  - mutation-testing
  - worker-shim
date: 2026-10-16
status: active
module: packages/daemon/internal/shim/shim.go, packages/daemon/internal/config/config.go
applies_when:
  - A wait is derived as "what a budget leaves" and the derivation reaches zero for some configuration the loader still accepts
  - The budget is set at one end of a two-binary contract (the daemon's file, the shim's flag) and the two can run at different releases
  - A new constant is the bound and a test must pin why that number and not a larger one
related_issues:
  - "LEGION-638"
  - "sjawhar/legion#1981"
---

# A derived wait gets a floor; the daemon refuses at load what cannot fit it; the downstream binary stays permissive; the floor is proven minimal

Extends `docs/solutions/daemon/a-background-task-the-shim-owns-ends-on-the-stop-signal-and-is-waited-for-inside-the-pods-real-stop-grace-LEGION-629.md`.
That note derives the warm-up drain as `stopGrace − Grace − drainTimeout` and states the known
edge: zero at a stop grace of 6 s or less. This is the shape that closes such an edge.

- **Floor the wait at what the waited-for thing needs** (`MinWarmUpDrain`: the child's own kill
  grace plus one second to reap it and release the lease), and derive the smallest budget the whole
  stop fits (`MinStopGrace = Grace + DrainTimeout + MinWarmUpDrain`). Then refuse, where the budget
  is configured, any value under it — at configuration load, so `legion start`, `--check-config` and
  `probe-boot` all say so before a pod runs. Refuse only where the edge exists (`runtime:
  kubernetes`; a tmux shim gets neither the budget nor the task). Record the rejected alternative
  with its reason (clamping the agent's grace to half the stop grace: still does not fit at the
  smallest accepted value, and shortens the agent's exit window).
- **Keep the downstream binary permissive.** The shim's `--stop-grace` keeps refusing only a
  non-positive value: an older daemon may still hand a smaller grace, and flooring the wait there
  costs at most a lease left for a minute, where a refusal would fail every launch of that
  deployment. A floor lands at the configuring end of a two-binary contract, never at both.
- **Derive the bound and its message from the owning package's exported constants**, and print the
  arithmetic (`5s + 1s + 3s = 9s`) so an operator fixes the file without the docs and the test
  asserts the sentence without a second copy of the sum. Export the sibling's unexported term rather
  than back-deriving it. Then count the prose sites that restate the numbers (here four: the agent
  guide, the operator docs, the troubleshooting row, the flag help) before writing the fifth.
- **Prove the floor minimal in the test.** Beside the hand-picked values: the floor holds for every
  input, the stop fits inside the budget from `MinStopGrace` up, **and does not fit one unit below**.
  Without the last assertion a larger floor also passes and the number is unexplained. Name in the
  test's doc comment the two mutations it catches (the formula without one term; the floor at zero)
  so a reviewer re-runs the control from the test alone.
- **A floored wait needs gating and a fake clock.** Once the wait can no longer be zero, every
  process exit would register a 3 s timer; return at once when nothing was started, and take the
  wait from the component's `Clock` so the tests drive it. Give the test's grace and the floor
  different durations: when two waits are the same length a fake clock tells them apart only by
  registration order, and the test can pass for the wrong reason.

## Evidence

- LEGION-638 (sjawhar/legion#1981): `warmUpDrain` returns `max(stopGrace−grace−DrainTimeout,
  MinWarmUpDrain)`; `internal/config` refuses `worker_stop_timeout_seconds < shim.MinStopGrace`
  under `runtime: kubernetes` with the arithmetic in the message; `--check-config` on 8 exits 1, on
  9 says `Config OK`, and 8 under tmux says `Config OK`. The old formula built through
  `go build -overlay` with `--stop-grace 6s` left the lease behind after a stop mid-build (`still
  running 0s … leaving it`); the floored shim at 9 s released it 0.05 s after the signal, and 5.61 s
  after it with an agent that ignored SIGTERM.
- `go test -overlay` against the drain test: dropping `− DrainTimeout` fails the fit invariant at
  the floor (`can take 10s, past the stop grace 9s`); a zero floor fails the floor invariant
  (`warmUpDrain(6s, 5s) = 0s`) and the one-unit-below assertion (`8s … inside a stop grace under
  MinStopGrace 9s`).
- The shim's floored-wait test ran at grace 3 s with `MinWarmUpDrain` 3 s, told apart by order
  (the kill timer registers before the child exits, the drain only after); the reviewer's
  Fast-follow asks for a run at `DefaultGrace`/`MinStopGrace` too.
