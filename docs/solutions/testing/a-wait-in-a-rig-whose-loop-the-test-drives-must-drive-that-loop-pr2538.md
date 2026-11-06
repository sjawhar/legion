---
title: "A wait in a rig whose loop the test drives must drive that loop, and a single tick plus an immediate assert is a race"
category: testing
tags:
  - flaky-tests
  - reconciler
  - held-clock
  - fake-runtime
  - daemon-sweep
date: 2026-11-02
status: active
module: packages/daemon/internal/daemon
related_issues:
  - "INFRA-484"
  - "sjawhar/legion#2538"
---

# A wait in a rig whose loop the test drives must drive that loop, and a single tick plus an immediate assert is a race

Complements docs/solutions/testing/await-the-event-not-a-tick-budget.md, which is about waiting on
real I/O. This is about a loop the test itself owns.

- If the production decision happens only inside a loop the test calls by hand, a polling wait
  **cannot** succeed by polling: nothing will ever drive another iteration. Call the loop between
  polls, inside the bounded wait.
- If the decision can be **deferred** by a live re-read — a reconciler that re-reads state and
  leaves the decision to its next pass — then one iteration plus an immediate assert asserts a
  race. Sweep until the expected count is reached.
- Assert the **negative** with one read and no wait. No deferral turns nothing into something, so
  a wait there only hides a wrong answer for the length of its bound.
- Classify every wait in such a rig, and say in a comment which class each is. A condition a
  posted event or a direct call brings about is an ordinary poll; only a condition the loop itself
  decides needs the loop.
- Before copying this pattern, re-check its three preconditions: that an extra iteration is free
  (a held clock, so no re-task or notice bound is crossed), that a decision already posted has
  time to land before the next iteration reads it half-applied, and that the loop is idempotent
  for the state you are waiting on.
- **Record a rejected alternative with a measurement, not an opinion.** The next reader will
  propose it again.

## Evidence (INFRA-484, sjawhar/legion#2538)

The daemon's orphan sweep decides update rounds. In the test rig the clock is held and the sweep
runs only through `rig.tick()`, so a wait that polls without ticking can only time out.

A subtest ran one tick and then polled for the round to end. The sweep reads the claims through a
mask that treats a claim whose machine is mid-decision as not having answered, so that one tick
could decide nothing and the decision moved to a sweep that never came. It failed about 3 runs in
60 locally and reddened CI's required lane at run 41393622961: `timed out waiting for implementer:
its round to end`. The replacement count had the same shape at run 41340112838, which is why
`awaitReplacement` already swept.

`awaitSwept` sweeps between polls. Its **first poll sweeps nothing** on purpose: a decision the
last sweep posted is in flight rather than undecided, and a sweep that read it half landed would
decide the round twice — replacing the pod again, or re-asking every worker of a round whose end
had reached one claim and not its neighbour.

The rejected alternative is recorded in the helper's own comment with its measurement, because it
looks obviously better and is not: `settleAll` before each tick — waiting for every machine to
settle — **times out that same test in every subtest**, where the skipped poll passes it 100 runs
out of 100. It nests a bounded wait per machine inside the outer wait, and a sweep that reads only
fully settled machines does not reach the decision these waits exist for.

Sweeping alone did not fix the subtest either. The deeper cause was that the test drove its tick
while a worker's ask was still being written, so that claim was not yet a member of the round and
the sweep took a different branch. The wait made the symptom go away; the fix was to wait for
every seeded role's round before the step under test.
