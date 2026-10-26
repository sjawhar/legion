---
title: "A proxy that serves a child takes a start-only context, is closed after the child is reaped, and its Close can end what it waits on"
category: daemon
tags:
  - context-cancellation
  - shutdown-grace
  - sigterm
  - bounded-close
  - drain
  - command-loop
  - deadlines
  - launcher
date: 2026-10-25
status: active
module: packages/daemon/internal/sessionproxy
related_issues:
  - "INFRA-271"
  - "sjawhar/legion#2179"
---

# A proxy that serves a child takes a start-only context, is closed after the child is reaped, and its Close can end what it waits on

Extends docs/solutions/daemon/a-sidecar-loop-that-serves-a-foreground-child-lives-until-the-child-exits-not-until-the-parents-signal-context-ends-LEGION-668.md.

That note covers a loop the parent starts itself. This note covers a component built by a
`Start(ctx, …)` constructor, and the three ways such a component broke in turn:

- **The ctx a constructor takes bounds the setup only.** Build the serving context from
  `context.WithCancel(context.WithoutCancel(ctx))`. The constructor's own blocking work (a bind, a
  proof dial) uses `ctx`, and only `Close` ends what is left. If the serving work hangs off the
  caller's ctx, then the container's SIGTERM, which ends the launcher's Run context, closes the
  component while the child is still in its stop grace. Prove this with a child that opens a
  **fresh** connection after SIGTERM, through both stop paths: the daemon's stop and the
  container's shutdown. A child that only holds an open connection proves nothing.
- **Close it where the child is reaped, after the leftovers are cleaned up, and let the stop wait
  on that.** In `manager.wait` the order is `cmd.Wait` → `cleanExitedGeneration` → `Close` →
  remove the directory → `close(done)`. `endGeneration` returns only on `done`, so a second
  `Close` after `stopActive` is dead code. Before writing such a call, check the existing path
  for it.
- **A Close that waits for in-flight work must be able to end that work.** A `WaitGroup` over
  `io.Copy` goroutines that Close cannot close is unbounded: any holder that never hangs up
  vetoes it. Track each pair under a mutex. Set a `closing` flag first, so that a pair that
  registers late is closed at registration. Drain for a bounded time, then close both ends of
  what is left, and log how many. Then disable the flag check by mutation and confirm a test goes
  red. Here nothing caught it until the tester added `close_race_test.go`. Close still waits for
  any dial that ignores its cancelled context. So the drain bound holds only while every dial
  honours cancellation, and the comment must say that condition rather than promise more than
  the code does.
- **A blocking call made while a one-command-at-a-time reader handles a command gets its own
  deadline, derived in code from the peer's wait on that command.** If the call outlives that
  wait, the peer gives up and its next command (the Stop) also goes unread. Here the daemon waits
  until the launcher's connection deadline plus its termination grace (`relaunch.go`). It passes
  that same grace to the launcher as `--stop-grace` (`manifest.go`), so `StopGrace/2` always
  fits. Cite both lines in the comment. Report the bound as the cause only when the bounded ctx
  fired while the parent was still alive.
- **State the lifetime bound per case, as the signals actually reach.** In a pod's PID 1, a
  stop's SIGKILL reaches the whole PID namespace, so a stop costs one grace. A child that exits
  on its own gives its leftovers one more grace. Off PID 1, signals reach only the child's
  process group, so only the drain bounds a foreign holder.

## Evidence

- The tester's red test `TestASessionDatabaseServesItsChildThroughTheChildsShutdownGrace`
  (`internal/launcher/session_shutdown_test.go`): with the proxy on Run's context, a child's
  shutdown-time `psql` through the socket failed with `No such file or directory` and wrote 0
  rows. After `18b29e17673c`, `INSERT 0 1`.
- Review round 1 at `12217616312f` found the remaining two holes:
  - The proof dial had no deadline. Live, with the real connector's Admin API held, the start got
    no answer within 10 s. After `00b13caf9d26` it was refused at 5.11 s with
    `no answer within 5s`, and the queued Stop was answered at once.
  - Close waited on copies it never closed. Off PID 1, a setsid'd `psql` in `pg_sleep(600)` left
    Run stuck for more than 20 s. After `523b2d19173f` Run returned in 1.01 s and logged
    `closed 1 connections … still open after 1s`.
- The resume lookup still runs on `context.Background()` for up to 30 s (`launcher.go`,
  `sessionWritten`). That leaves the same rule half applied, and the review accepted it as a
  recorded risk. The tester drove it: the start was refused at 30 s and the Stop was answered in
  the same instant.
- Retro's fresh reader found that `Close`'s comments promised it "never waits on a client longer
  than Drain". `close_race_test.go`'s `heldDialer` ignores its context and holds Close past the
  drain. A send-back narrowed the comments to that condition (`34b2b7a999e4`). The same round
  corrected `docs/kubernetes.md`, which said every start refusal names the generation and the
  instance; only the proxy's does.
