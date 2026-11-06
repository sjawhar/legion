---
title: "A bash background loop ends when its starting shell is gone, and checks its stop flag after every foreground command before it acts"
category: best-practices
tags:
  - bash
  - signals
  - trap
  - background-process
  - orphan
date: 2026-11-03
status: active
module: packages/envoy/loadtest/cluster
related_issues:
  - "sjawhar/legion#2462"
---

# A bash background loop ends when its starting shell is gone, and checks its stop flag after every foreground command before it acts

Extends `docs/solutions/infra/a-shell-script-that-is-a-containers-pid-1-traps-term-sleeps-under-wait-and-exits-when-its-helper-dies-INFRA-560.md`.
That note says a shell runs a trap only after a foreground command returns, and so sleeps under `wait`.
This one covers a watcher that acts on what it reads.

- **Audit every process that can outlive the command that started it, as a class, in one round.**
  This covers `&` watchers, daemons, and command substitutions such as `rc=$(wait_for_job)`, which a SIGKILL of the step shell does not reach.
  Give each one the starting shell's pid and its `/proc` start time. Each poll ends it, recording nothing and stopping nothing, once that process is gone or is another process now.
  Names of jobs, pods and steps repeat across runs, so an orphan of one run acts on the next run's job of the same name.

- **Use one TERM trap for the whole function, and have it only set a flag and end the nap.**
  `trap 'stopped=1; [ -z "$nap" ] || kill -- "$nap" 2>/dev/null' TERM`.
  Swapping traps around an act loses the TERM that lands between two `trap` lines.

- **Check the flag after each foreground command and right before every act.**
  A TERM that arrives during a foreground read (a `kubectl exec`) runs its trap only when the read ends.
  If the next line acts on that reading, the watcher acts after the step it watched has ended.
  It then stops a finished job, records a false failure, and ends browser typing before the check that needs it.
  Check the flag as soon as `sleep N &` is forked, after `wait "$nap"`, and after the read, before the act.

- **Bound the stop that waits for the watcher to leave, then kill it and log the kill.**
  A step must not hang in its cleanup because a signal was lost.

## Evidence

sjawhar/legion#2462, `packages/envoy/loadtest/cluster/cluster.sh`: `step_gone` (line 908), `pg_disk_watch` (line 1384, its flag checks at 1388, 1392, 1395 and 1408), `pg_disk_watch_stop` (line 1444).
- The orphan class was found three rounds running, each time in a different process: the heartbeat (round 1), the disk watcher (round 2), and `runner_wait`'s command substitution (round 3). It was fixed in 2f6dc193, 005e8aa7 and 7604610d, converging on `step_gone`.
- The lost TERM in a trap swap is thread PRRT_kwDORFy7ds6yamNf (round 8).
- The TERM during the volume read is PRRT_kwDORFy7ds6yntgS, and the nap race is PRRT_kwDORFy7ds6yntgc (round 10, fixed in f02759fd).
- The tester drove the TERM during the read live on one copy. With the read made to take 60 s, f02759fd wrote no `step-failed.json`, while cfe5a7b7, on the same copy, recorded a false `pg_disk` failure.
