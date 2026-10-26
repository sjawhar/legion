---
title: "Decide a signal's owner from state, never from a select over two ready channels — and measure the race with signals the kernel cannot merge"
category: best-practices
tags:
  - go
  - signals
  - sigterm
  - graceful-shutdown
  - concurrency
  - race-measurement
date: 2026-10-25
status: active
module: packages/envoy/cmd/broker
related_issues:
  - "INFRA-236"
  - "sjawhar/legion#2162"
applies_when:
  - "a Go service handles a second SIGTERM differently from the first"
  - "boot work runs on a context no signal cancels"
  - "a harness sends two signals to measure which path a process takes"
---

# Decide a signal's owner from state, and measure with signals that cannot merge

- **Never choose an owner with a `select` over a state channel and an event channel.** Go picks
  uniformly at random among ready cases, so `select { case <-serving: …; case <-second: … }` takes
  the wrong branch about half the time whenever both are ready — which is exactly the case the
  code exists for. Take the events first, then read the state once with `select`/`default`. The
  branch then comes from the state at the instant the event arrived, and a channel that only ever
  moves one way makes that reading stable.
- **Extract the decision into a function.** A test can then drive it thousands of times with both
  inputs already ready — the moment a `select` would coin-flip — where a test of the wired-up
  process can only hope to hit it.
- **Boot work on an uncancellable context needs an escape registered before it starts.** Moving a
  load onto `context.WithoutCancel` so it survives the drain also makes it survive the operator:
  one signal is noted and nothing happens. Register the second-signal path before any boot work,
  and say in the comment that a single signal does not interrupt that call, so the next reader does
  not rediscover it as a hang.
- **A fix for one concurrency defect is where the next one is introduced.** The coin flip above was
  introduced by the fix that gave the boot its escape: the fix added a second reader of a channel
  that had one. Any change to who reads an event needs its own adversarial test in the same push.
- **Measure a second-signal path with `SIGTERM` then `SIGINT`.** A pending non-realtime signal is
  not queued, so two `SIGTERM`s sent back to back merge in the kernel and most of your runs never
  deliver a second signal at all. Use two different signals the handler treats alike.
- **A replica of a race is a lower bound on it.** A harness that reproduces a race in a copy of the
  logic measures less than the real binary does, because the real path puts more work between the
  two steps. So a replica that finds a race has found a worse one than it measured, and a replica
  that finds none has found nothing. Report the exercised count, not the run count.

## Evidence

The Envoy secrets broker's stop path, INFRA-236 / sjawhar/legion#2162
(`packages/envoy/cmd/broker/main.go`).

- The watchdog chose between handing a second signal to the drain and exiting without one by
  selecting over `serveStarted` and the signal channel. Measurements of the same defect, in order
  of honesty: a reviewer's replica with two `SIGTERM`s, **2 in 300**; the real pre-fix binary the
  same way, **10–11 in 300**; the real binary with `SIGTERM` then `SIGINT`, **39 in 100**. Each
  earlier figure is a lower bound of the next, and the merged-signal figures understate it by an
  order of magnitude.
- The fix is `secondSignalFindsServing`, which takes both signals and then reads `serveStarted`
  once with `select`/`default`. Its row asks 500 times with both inputs ready and requires the
  drain every time; run against the previous body with `go build -overlay`, it answers
  `exit 245 times in 500`. On the real binary, 600 runs of which **354 genuinely exercised** a
  second signal at a serving broker all drained and exited 0.
- The comment that survives says what the check establishes — that the broker had not begun serving
  when the second signal was read — rather than what the exit finds, because nothing orders the
  exit against the two statements that may close the state channel. The window is deliberately not
  synchronised, and the comment says so, so a later reader does not add a lock to a window that
  does not need one.
