---
title: "An ownership guard answers ours, another's or unknown, is read once per decision, and only unknown retries without a bound"
category: best-practices
tags:
  - lock
  - fail-closed
  - polling
  - bash
  - kubernetes
date: 2026-11-03
status: active
module: packages/envoy/loadtest/cluster
related_issues:
  - "sjawhar/legion#2462"
---

# An ownership guard answers ours, another's or unknown, is read once per decision, and only unknown retries without a bound

Extends `docs/solutions/daemon/a-lease-holders-heartbeat-ends-only-on-another-token-an-unreadable-read-is-retried-inside-the-stale-budget-and-the-release-shares-no-predicate-with-it-LEGION-638.md`.
That note splits one predicate into a fail-closed release and a fail-open refresh.
This one is about a guarded act that a poller repeats, such as stopping a job that keeps loading a shared resource.

- **Give the guard more than two answers, and let only the caller collapse them.**
  The answers are: the resource is ours; the lock names another owner; no read answered; and, for a guarded act, the act itself failed.
  A guard that answers only "ours or not" makes every poller choose between two wrong outcomes.
  If "not" stops the poller, it stops watching for the rest of the run on one failed read.
  If "not" retries, it retries for ever when the lock names another owner.

- **Read the owner once per decision, and act on that one answer.**
  After the decision, never call a helper that reads the owner again.
  A second read that fails turns the caller's "ours" into a refusal, so the act is skipped without a sound.
  Put the read and the act in one function that returns the answer: `runner_stop NAME [SIGNAL]` returns 0 sent, 1 another run, 2 unread, 3 kubectl failed.
  Each poller is then one `case` on that answer.

- **"Another owner" ends the poll; "unknown" acts on nothing this time and asks again at the next poll.**
  A failed read says nothing about whose resource it is.
  The volume is still filling, and the job is still this run's to stop once a read answers.

- **Bound every other retry from when its step fell due.**
  Past a deadline, each stop (TERM, then KILL, then waiting for the exit status) gets one grace, counted from when it fell due.
  When the act fails because the target cannot be reached, the wait ends non-zero once that grace has passed, with a note naming the target.
  Only "unknown owner" retries without a bound, since an API server no read reaches blocks every run anyway.
  A wait that polls for an exit status from a pod that is gone never ends, and it keeps the lock's heartbeat alive, so no other worker can take the namespace.

- **Quote the last failed read's own error in the refusal note.**
  Without it, an operator cannot tell a `forbidden` from a refused connection.

## Evidence

sjawhar/legion#2462, `packages/envoy/loadtest/cluster/cluster.sh`: `copy_is_ours` (line 869), `runner_stop` (line 1679), `runner_wait` (line 1729), `pg_disk_watch` (line 1384).
The guard went the wrong way four rounds running:
- round 6 made it fail closed (32592e51);
- round 7 found that the disk watcher then stopped watching after one unreadable lock (thread PRRT_kwDORFy7ds6yTeRa, fixed in d9d38377 with answer 2);
- round 8 found that `runner_stop` read the lock again and turned that 2 into a plain 1 (PRRT_kwDORFy7ds6yamNd, fixed in 83986ea7);
- round 9 found that a wait past its deadline never ended once the runner pod took no exec (PRRT_kwDORFy7ds6yf_x4).

Round 9 replaced three hand-written ladders with the one four-answer `runner_stop` (cfe5a7b7). Round 10 folded the TERM and KILL stops into one `signal`/`due` pair with the same bounds (f02759fd).
The tester drove the unreachable-runner case live in namespace `loadtest`. At cfe5a7b7 the wait ended at 120 s and the heartbeat froze. Under ce1289aa's code the same injection retried 25 times and renewed the lock until it was stopped by hand.
