---
title: "A sidecar loop that serves a foreground child lives until the child exits, not until the parent's signal context ends"
category: daemon
tags:
  - signal-handling
  - context-cancellation
  - refresh-loop
  - controller-start
  - oh-my-pi
  - process-group
date: 2026-10-10
status: active
module: packages/daemon/cmd/legion
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A sidecar loop that serves a foreground child lives until the child exits, not until the parent's signal context ends

- A CLI that runs a child in the foreground and a goroutine that serves that child (a credential
  refresh, a heartbeat) derives the goroutine's context from `context.Background()` and cancels
  it when the child's `Run()` returns, never from the context `signal.NotifyContext` cancels on
  SIGINT/SIGTERM. A terminal Ctrl-C reaches the whole foreground process group: a child that
  handles the signal itself (Oh My Pi treats it as its own and keeps running) outlives the parent's
  context, and a loop bound to that context dies while the child still depends on it.
- The stop function cancels and waits (`<-done`) so the process never exits mid-write; the loop
  writes nothing to the terminal the child owns and logs to a file instead.
- This is the opposite coupling from a shim that owns its child: there the background task ends
  on the stop signal, inside the pod's stop grace
  (docs/solutions/daemon/a-background-task-the-shim-owns-ends-on-the-stop-signal-and-is-waited-for-inside-the-pods-real-stop-grace-LEGION-629.md).
  Which process owns the lifetime decides which signal the loop follows.

## Evidence

`legion controller start`'s refresh loop (`packages/daemon/cmd/legion/controller_credential.go`,
`startControllerGitHubRefresh`) first took `controllerStart`'s ctx, which `main`
(`cmd/legion/main.go`, `signal.NotifyContext(…, os.Interrupt, syscall.SIGTERM)`) cancels on
Ctrl-C while `controller.go` deliberately leaves Oh My Pi unbound to it ("a Ctrl-C reaches Oh My
Pi through the terminal's process group and is its to handle"). Caught in review of the worker's
diff before the first push (implement.json deviations); the e2e `ctrl-c-reaches-omp-not-the-cli`
checkpoint in `scripts/e2e/controller-start-tmux.sh` is the live shape of the same fact.
