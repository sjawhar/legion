---
title: "A shutdown budget bounds only the wait, so name what cuts the work and sum its own bounds"
category: best-practices
tags:
  - shutdown
  - go
  - context
  - graceful-stop
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2404"
---

# A shutdown budget bounds only the wait, so name what cuts the work and sum its own bounds

## The rule

1. A `context.WithTimeout` handed to a stop bounds how long the caller waits for it. It does not
   stop work the stop started in its own goroutines with their own bounds. Add up those bounds
   (every grace, flush, linger, drain and write) and write the sum beside the budget, including
   the worst case when each one runs to its limit.
2. When the sum can exceed the budget, say what happens to the work still running: which later
   step of the shutdown ends it (a connection drain, a close, `os.Exit`). Do not write "the
   runtime kills it". If the process's own steps cut the work, a longer grace period in the
   runtime changes nothing, and an operator who raises it based on your text gains nothing.
3. Make what an overrun can lose safe. Then write down what it is: here, a mark whose absence
   leaves the next owner its long, safe hold.
4. When you add a step to a stop (here, a mark write), update every place that states the sum: the
   code comment, the README and the changelog.

## Evidence

- `packages/envoy/cmd/listener/main.go:1522` gives the receive stop 10 s
  (`shutdownCtx`). Its parts come to 11 s, and 12.25 s when the flush and the release run to their
  bounds: a 3 s grace, a 1 s cancel wait, a 3 s linger, a 2 s drain and a 2 s mark write.
- Past the budget the shutdown goes on to `server.Shutdown` (`:1528`), then `client.Drain`
  (`:1557`), which ends a lingering hand-back's subscription, then `os.Exit(1)`, which ends a mark
  write still in flight. The comment at `:1514-1520` says this.
- Round 7 of #2404 found the sum missing the mark write. Round 8 found the text blaming the
  runtime's kill. Round 9 found it naming `os.Exit` for what `client.Drain` does. Each finding
  was one line of prose, but a wrong line here misleads anyone sizing the pod's
  `terminationGracePeriodSeconds`.
