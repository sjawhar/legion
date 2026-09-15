---
title: "Four bash traps in a rig's lib.sh: a caller's `local` shadows a script-scope array in every callback, `exit` inside `$(…)` leaves only the subshell, a setsid'd child forms its group after exec, and only an argv (never a function) may front a recorded process"
category: testing
tags:
  - bash
  - dynamic-scoping
  - command-substitution
  - setsid
  - process-group
  - smoke-rig
  - kind-smoke
date: 2026-09-15
status: active
module: scripts/kind-smoke (lib.sh poll/start_process_group, checkpoints.sh readers, up.sh scrub_argv)
applies_when:
  - A generic bash helper (`poll`, `retry`, `with_timeout`) runs caller-supplied functions
  - A function called inside `$(…)` decides a verdict or calls `exit`
  - A script records the pgid of a `setsid`'d background command
  - Something is placed in front of a command whose pid gets recorded and later killed
related_issues:
  - "LEGION-26"
  - "sjawhar/legion#1114"
---

# Four bash traps in a rig's lib.sh

Each of these cost the kind smoke a round or a failed CI run. None is exotic; all four are easy to
write without noticing.

## 1. A caller's `local` shadows a script-scope array in every function it calls

Bash is dynamically scoped: a `local` in a function is visible to every function that function
calls. `checkpoints.sh` declares `declare -A budget=([kill-resume]=… …)` at script scope, and
`lib.sh`'s `poll` begins `local budget="$1" what="$2"` and then runs the caller's predicate.
Inside the predicate, `${budget[kill-resume]}` now refers to `poll`'s scalar; bash parses the
subscript as arithmetic on the name `kill-resume` and dies with `kill: unbound variable` under
`set -u`. Nothing in the predicate's own text looks wrong.

Fix: read the value into a plain variable **before** the `poll` call and use that inside
(`resume_budget="${budget[kill-resume]}"; poll "$resume_budget" … try_kill_resumed`). Rule for a
helper that runs callbacks: give its locals names no callback could want (`poll_budget`), or pass
what the callback needs as arguments.

## 2. `exit` inside `$(…)` leaves only the subshell

A reader such as `issue_status` ran inside a command substitution and called `failed …` (which
prints `CHECKPOINT <name> FAILED: …` and exits) when Dispatch did not answer. The `exit` ended the
substitution's subshell; the caller in the main shell went on with an empty string and printed a
second verdict — the reviewer reproduced `CHECKPOINT admitted FAILED: Dispatch did not answer …`
followed by `CHECKPOINT admitted OK`.

Fix: a function that runs inside `$(…)` returns status and prints its value, nothing more; the
caller in the main shell decides (`… || { dispatch_miss "issues/$key"; return 1; }`, where
`dispatch_miss` only sets the poll's `last` message). The harness pins "exactly one `CHECKPOINT `
line per run" (`expect_verdict`), so a second verdict can never pass again.

## 3. A setsid'd child is not yet its own group leader when `$!` returns

`setsid cmd &` forks a child that still shares the parent's process group; it moves into its own
group only after it has exec'd the `setsid` binary and that has called `setsid()`. Reading
`/proc/<pid>/stat` field 5 right after `&` can return the parent's pgid — which the first pushed
head then recorded as the port-forward's group, so `down.sh` would have signalled the rig's own
group. It never happened on the dev box; the first CI run failed with `port-forward did not start
in its own process group`.

Fix (`start_process_group`): poll `process_group_id "$pid"` until it equals `$pid` (100 × 50 ms)
before writing the record, and fail naming both ids if it never does. The harnesses fake every
external binary except `setsid`, precisely so CI exercises this. The harness-side twin of the trap
(a fixture read too early) is in `bash-harness-cases-that-pass-for-the-wrong-reason.md`, § 3.

## 4. Only an argv may front a recorded process

The environment scrub was first a shell function wrapping `env -u …` placed in front of
`start_process`. Backgrounding a function forks a bash subshell, so the recorded pid was that
subshell: its `/proc/<pid>/cmdline` read `bash scripts/kind-smoke/up.sh`, its environment was the
unscrubbed launcher's, and `down.sh` killed the wrapper and left the listener and Dispatch binaries
bound to their ports (the `legion26d` run). `scrub_argv` now fills an array —
`scrub=(env -u NAME …)` — and the call is `start_process listener "${scrub[@]}" "$bin" …`: `env`
execs the binary in place, so pid, environment, and signals are the binary's; it also works after
`setsid`, which execs a binary and cannot run a function. `up.test.sh` refutes `up.sh` in the
recorded pid's `/proc/<pid>/cmdline`.

## Related

- `docs/solutions/testing/bash-harness-cases-that-pass-for-the-wrong-reason.md`: `set -e` inside
  `if`, `local x="$(cmd)"`, and the fixture side of the setsid race.
- `docs/solutions/testing/set-e-exempts-a-negated-command-and-non-final-list-operands-a-refute-accepts-exactly-exit-1.md`:
  the harness rules these scripts are tested under.
