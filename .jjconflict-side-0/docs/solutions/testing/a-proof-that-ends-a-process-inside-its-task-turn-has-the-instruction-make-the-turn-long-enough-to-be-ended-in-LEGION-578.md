---
title: "A proof that ends a process inside its task turn has the instruction make the turn long enough to be ended in"
category: testing
tags:
  - live-proof
  - stage4b
  - supervise-budgets
  - deaths-with-work
  - host-load
date: 2026-10-16
status: active
module: scripts/e2e/stage4b-sandbox-tree.sh
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1866"
---

# A proof that ends a process inside its task turn has the instruction make the turn long enough to be ended in

- The daemon charges a death "with work outstanding" only when the end lands before the task's
  turn is over (`packages/daemon/internal/supervise`); an end that lands after charges nothing, and
  the relaunch has no task. A proof that drives a hold by ending launches therefore needs each task
  turn to outlast the end, and the end is not instant: a `kubectl exec … kill 1` took up to a minute
  under host load 117–234 on 32 cores, while a resumed session reading its role documents and
  replying WAITING ends its turn in seconds.
- Make the instruction create the turn: the proof's deployment instructions have the role run
  `sleep 300` as one bash call (tool timeout 600 s) before it replies WAITING on the daemon's task
  line, and the re-arm task a missed end delivers asks whichever role receives it for the same
  sleep, directly, in the task text. Two texts that must agree (the instructions' clause and the
  re-arm task) are two places a future edit can break; pin both where a unit can reach them.
- Send the kill the moment the claim is ready with its task outstanding, as before; the sleep makes
  the turn long, not the hold slow. Budget the first turns it lengthens: every implementer's first
  turn in the run grows by five minutes, and the fixed bounds that wait on an implementer's first
  action (its pull request, its CI-red fix, its completion) must still hold.
- Raising the miss budget hides the race; it does not fix it. A missed end is a diagnosis
  (`the end of <process> came after its task's turn had ended … gave <claim> a new task`), and more
  than one or two in a loop is a run the host is too slow for, not a flaky daemon.

## Evidence

sjawhar/legion#1866 (LEGION-578): the third live Stage 4b run, at 2e0e4171, passed 34 checkpoints
and failed at `controller`: `3 ends of legion-legsmoke-legsmoke-785-implementer came after their
task's turn had ended … 2 new tasks did not put an end inside a turn`. The runner log showed
launches 1 and 2 charged (the kill inside the turn), launches 3 and 4 missed (the relaunch's short
turn ended first; each given a new task), and launch 5 waiting 66 s for the kill to land. The
coordinator read it as the check's timing under load, not the daemon; the architect ruled it fixed
in the pull request (`main`'s harness, not a LEGION-578 criterion) rather than re-rolled. The fix
(13cb9ce7) put the `sleep 300` clause in the proof's instructions and the re-arm task; the passing
run reported `LEGSMOKE-796 is held after 3 ended implementer launches, 0 of them after their task's
turn`, each death charged `1 of 3`, `2 of 3`, `3 of 3`. Review round 1 then found the re-arm task
citing "an implementer" where `deaths-with-work` sends it to a planner (thread
PRRT_kwDORFy7ds6tAG7u); d25908b1 addresses whichever role receives it. `stage4b-hold.test.ts` pins
the instructions' reading sentence in that task but not the sleep clause, and no run has delivered
the new text to a role yet — both recorded on the pull request under *Not proven / risk*.
