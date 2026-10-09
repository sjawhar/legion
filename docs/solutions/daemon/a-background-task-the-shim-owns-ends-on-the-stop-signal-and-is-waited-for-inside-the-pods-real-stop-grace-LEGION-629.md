---
title: "A background task the worker shim owns ends on the stop signal, not on its child's exit, and is waited for inside the pod's real stop grace — the one the launcher passes, never a sibling's fallback constant"
category: daemon
tags:
  - worker-shim
  - goroutine-lifetime
  - process-group
  - sigterm
  - stop-grace
  - terminationGracePeriodSeconds
  - worker_stop_timeout_seconds
  - codegraph
date: 2026-10-08
status: active
module: packages/daemon/internal/shim/shim.go
applies_when:
  - The shim (or any process whose exit is coupled to a child's exit) starts a goroutine with a side effect to undo (a lease, a lock, a temporary file)
  - A stop is one SIGTERM to the whole process group, so the child, the goroutine's own subprocess and the owner all go at once
  - A wait or drain bound is being chosen for a shutdown path, and a nearby constant looks like the stop grace
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
---

# A background task the shim owns ends on the stop signal and is waited for inside the pod's real stop grace

The worker shim's process ends when its Oh My Pi does (`reap` → `finish`). A goroutine the shim
started beside the agent — the CodeGraph warm-up, which holds a lease on the shared workspace —
dies with the process, its deferred release never run, when the launcher's one SIGTERM to the
role's process group ends Oh My Pi, the `codegraph` child and the shim within the same half
second. The role's relaunch then reads a live-looking lease and skips its own warm-up for a
minute. Rules:

- **Bind the task to the owner's lifetime, not the child's.** The task runs under a context the
  shim ends itself (`s.warm`, a child of the shim's loop) and Run waits for the goroutine to
  return before it returns the agent's exit status (`drainWarmUp`). A `WaitGroup` the owner never
  waits on is not a lifetime.
- **End it on the stop signal, not on the child's exit.** `terminate` cancels the warm-up the
  moment the child is told to stop, so an agent that spends its whole grace ignoring SIGTERM costs
  the stop nothing more; the task's own subprocess is SIGTERMed on that cancel (`cmd.Cancel`) and
  killed after its own short grace (`cmd.WaitDelay`, 2 s for CodeGraph 1.5.0, which leaves in
  about 0.3 s).
- **Bound the wait by the stop grace the runtime actually gives this process, threaded in
  explicitly.** A pod's launcher is killed at `terminationGracePeriodSeconds` =
  `worker_stop_timeout_seconds` (10 s by default), which it also receives as `--stop-grace`; the
  launcher's own 30 s is only its unset fallback and a pod never leaves it unset. The shim now
  receives the same value (`launcherCommand` passes `--stop-grace`, `Config.StopGrace`), and the
  wait is what it leaves once the agent's grace and the stdout drain are spent:
  `max(stopGrace − Grace − drainTimeout, 0)`, 10 − 5 − 1 = 4 s with the defaults. A bound reasoned
  against a sibling component's constant was the reviewer's finding.
- **Declare the defers so the drain runs after the loop is cancelled**, and say so in a comment:
  Go runs defers in reverse, and a reader should not need to prove `run()` returns only after the
  loop ended.
- **Known edge, stated where the bound is derived:** at `worker_stop_timeout_seconds` ≤ 6 the
  formula is 0 and a mid-build stop again leaves the lease for up to a minute (a delayed warm-up,
  never a corrupt index). Clamping the agent grace to half the stop grace is the open fast-follow;
  until it lands, a deployment that lowers the stop timeout that far should expect the minute.

## Evidence

- LEGION-629 round 1 (tester): the branch's `legion worker-shim --pod-safety --warm-codegraph`
  wrapping the real Oh My Pi, `kill -TERM -- -<pgid>` 12 s into the build: Oh My Pi, the shim and
  both `codegraph` processes gone within 0.5 s, `.codegraph/legion-warm.lock` left with its last
  heartbeat's mtime, no `[legion]` line logged; the relaunch within the minute logged
  `skipped: another process holds`. A unit test of the goroutine alone had passed: it never
  modelled the owner exiting first.
- Round 2 fixed the join; round 3 (reviewer) found the drain's 10 s constant exceeded the pod's
  10 s grace once the agent's 5 s and the 1 s drain were spent (16 s worst case), reasoned against
  the launcher's 30 s fallback. With the grace threaded through, the worst case measured with an
  agent that ignores SIGTERM: killed at 5 s, shim exit 5.4–5.6 s after the group signal, lease
  released; the real Oh My Pi path 0.08–0.12 s.
- The config lineage, for the next reader: `worker_stop_timeout_seconds`
  (`internal/config/config.go`) → `Options.TerminationGrace` (`internal/runtime/sandbox/types.go`)
  → the pod's `terminationGracePeriodSeconds`, the launcher's `--stop-grace` and the shim's
  `--stop-grace` (`internal/runtime/sandbox/manifest.go`, `launcher.go`).

## Related

- `docs/solutions/daemon/a-file-presence-lease-on-a-shared-volume-exclusive-create-is-the-only-atomic-step-judge-a-lease-by-mtime-and-token-never-inode-LEGION-629.md`
  — the lease this task holds.
- `docs/solutions/testing/prove-a-cross-process-filesystem-property-on-the-filesystem-it-runs-on-and-through-the-real-stop-signal-LEGION-629.md`
  — the group-signal drive that found the round-1 defect.
