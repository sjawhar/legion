---
title: "A crashed phase worker is retired until something addresses it; only a crashed root is resurrected by the daemon itself — so a resume checkpoint crashes the root"
category: daemon
tags:
  - process-recovery
  - resync
  - worker-lifecycle
  - resume
  - kubernetes
  - smoke-rig
  - kind-smoke
date: 2026-09-15
status: active
module: packages/daemon/src/daemon/processes.ts (onWorkerClientClosed, markWorkerDead), resync.ts
applies_when:
  - Designing a check that crashes a Legion process and expects the daemon to bring it back
  - Reading daemon state after a worker pod or pane died and wondering why nothing relaunched it
  - Deciding which role a chaos or resume exercise may kill
related_issues:
  - "LEGION-26"
  - "sjawhar/legion#1114"
  - "LEGION-179"
---

# A crashed phase worker is retired until something addresses it

The kind smoke's resume checkpoint was first planned to kill "a pod" and wait for the daemon to
bring it back. Which pod matters, because the daemon recovers the two kinds of process by
different rules — as of the LEGION-26 branch, before LEGION-179 lands.

## A root architect: probed and resurrected within one resync interval

The periodic resync (`runResync`, `resync.ts`) emits a `probe` effect for every active,
ready-confirmed, located tree on every run. A root whose pod or pane is dead is resurrected under
its generation lock — the same OMP session with `--resume`, a new generation, the same tree volume
— with nothing else having to touch the tree. That is the designated backstop for "the root died
with nothing routed to it", and it is what `kill-pod-resume` proves: generation `N → N+1`, the
`--resume=<ompSessionFile>` argument on the replacement pod, the same session id in
`roles[<architect token>]`.

## A phase worker: retired, and relaunched only when addressed

When a confirmed phase worker's socket closes at runtime (`onWorkerClientClosed`), the daemon
makes one reconnect probe and then `markWorkerDead`: the locator is cleared, the OMP session file is
carried into `resumeSessionFile`, the worker queue is re-drained — and that is all. No `worker-died`
is published on that path, nothing is re-queued, no timer probes the claim again. The issue's
active phase still names the dead worker, and the architect is waiting for a `phase-complete` that
will not come. The claim is relaunched only when something addresses the role: the architect's
next `spawn_worker` (a locator-less claim with `resumeSessionFile` launches with `--resume`), or a
role-lane wake whose delivery exception or 404 makes `handleException` probe the locator dead and
resume the agent with a state-derived catch-up. In a run with no GitHub ingress nothing ever wakes
a killed tester, so it never returns.

That is why `SMOKE_KILL_ROLE` accepts only `architect` and prints `SKIPPED-BLOCKED` for anything
else, naming this rule, rather than failing a checkpoint the daemon was never going to pass.

## LEGION-179 changes the second rule

LEGION-179 ("a phase worker whose process dies mid-task is relaunched by the daemon itself —
periodic worker-claim probe + `--resume`, on both runtimes") was in retro when this was written.
Once it is on the image under test, a crashed phase worker is resurrected like a root and the
checkpoint can widen `SMOKE_KILL_ROLE`; until the image carries it, killing a worker in a smoke
run proves only that it stays dead.

## Related

- `docs/solutions/testing/crash-a-pod-from-the-node-kubectl-exec-kill-9-1-is-dropped-and-judge-the-kill-from-the-pod.md`:
  how the root's pod is crashed and how the resurrection is judged.
- `docs/solutions/daemon/retire-then-drain-a-pane-you-decide-not-to-prompt.md` and
  `docs/solutions/daemon/idle-expiry-timers-rearm-on-declines-that-change-while-idle.md`: the
  daemon's own retirements, which produce the same locator-less-with-`resumeSessionFile` shape on
  purpose.
