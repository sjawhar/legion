---
title: "A clean-up trap ignores further stops and runs each Go command under setsid -w, proven one signal at a time"
category: best-practices
tags:
  - signals
  - trap
  - shell
  - kubectl
  - kubernetes
date: 2026-10-25
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-386"
  - "sjawhar/legion#2196"
---

# A clean-up trap ignores further stops and runs each Go command under setsid -w, proven one signal at a time

Extends docs/solutions/testing/prove-a-stop-window-closed-by-sweeping-the-signal-across-it-beside-a-live-run-INFRA-232.md
and docs/solutions/best-practices/decide-a-signals-owner-from-state-and-measure-a-race-with-signals-that-cannot-merge-INFRA-236.md.

- **`trap - EXIT INT TERM HUP` at the top of a clean-up restores the default action for every
  signal, whatever started the trap.** A second stop, or one stop sent after a failed step or the
  work's end started the trap, kills the clean-up part-way. Nothing prints, not even its exit line.
- **`trap '' INT TERM HUP` protects the shell but not a Go child.** A Go program such as `kubectl`
  dies of a `SIGTERM` it inherited ignored. While it waits (`kubectl wait`, and `delete`'s own
  wait), its `signal.Notify` catches `SIGINT` and `SIGHUP` too, despite the inherited ignore.
  Before you state why a child survives, read the child's source for `signal.Notify`.
- **Run each command the clean-up must finish under `setsid -w`.** That puts the command in a
  session of its own, outside the process group the stop is sent to. In a shell without job control
  `setsid -w` does not fork, and the command's exit status passes through. Do not use `set -m`:
  it let a `SIGINT` end the trap.
- **Prove a claim about a set of signals one signal at a time, and name the launch.** Drive
  `TERM`, `INT` and `HUP` separately: a second stop, and one stop into a trap started by a failed
  step. State the launch's initial signal disposition, because a job started with `&` from a
  non-interactive shell starts with `SIGINT` ignored. A proof that drove only `SIGTERM` passed a
  line that `SIGINT` broke.
- **State the cost.** Of the stop signals, only a `SIGKILL` then ends a clean-up in progress, and
  it ends the shell alone. The command then running finishes in its own session, and a `flock`
  waiting on the script's lock waits for it.

## Evidence

sjawhar/legion#2196, the workload sequence in `docs/kubernetes.md`, run on the real block against
the cluster on 2026-10-25:

- **The block before the fix:** two `TERM`s 0.5 s apart, or one `TERM` into a failed step's trap,
  left the Job, its Service and its pod, with no `sequence: exit` line.
- **The ignore alone:** a second `TERM` killed the trap's `kubectl delete`, which printed
  `cleanup: the Job's delete failed`, and the workload stayed.
- **The ignore plus `set -m`:** the `TERM` cases passed, but a `SIGINT` into the trap left the
  workload, 5 of 5 times.
- **The ignore plus `setsid -w` on each `kubectl`:** `TERM` followed by `TERM`, `INT` or `HUP`
  ended `sequence: exit 143`, and one `INT` or `TERM` into a failed step's trap ended
  `sequence: exit 1`. Every case left nothing.
- **Why the sessions are needed:** the reviewer ran `kubectl wait --for=delete`, started with all
  three signals ignored, against a stub API server whose watch never answered. It ended on each of
  `INT`, `HUP` and `TERM`.
