---
title: "A status endpoint names a new leader before that leader logs it, so the log is a condition of the wait"
category: testing
tags:
  - race
  - log-assertions
  - polling
  - raft
  - deterministic-tests
date: 2026-11-02
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "sjawhar/legion#2639"
---

# A status endpoint names a new leader before that leader logs it, so the log is a condition of the wait

Extends docs/solutions/testing/force-a-lost-update-race-with-a-wrapped-jetstream-context-and-wait-for-the-log-line-the-state-precedes-pr2362.md,
which says an assertion on a log line waits for it with the same bound as the state wait. What is
new here is the direction of the gap and where the conjunct goes.

- **When a system reports a state through an endpoint and also logs it, the log is the later of
  the two, not the earlier and not the same instant.** The variable the endpoint reads is set on
  the state change itself, while the log waits for whatever the process does next. Measure the gap
  before you write either order into a test.
- **Put the log count inside the bounded wait, as one more conjunct, when the wait helper fails
  the test on timeout.** Put it after the wait only when the helper can return without its
  condition, which is the case
  docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md states
  ("Any bounded wait needs a hard assertion that the awaited thing actually happened"): that rule
  is about a helper that gives up silently. Read the helper before you choose. A second read after
  a helper that already fatals adds only a race.
- **Count the line, do not search for it.** Take the count before the act and require the count to
  rise, so a line the same process wrote in an earlier phase is not read as this act's.
- **Name a compound wait after every fact it waits for**, so its timeout says which one was
  missing rather than naming only the first.
- **A state whose lifetime is shorter than your poll period cannot be asserted by sampling it.**
  Assert the durable trace the system had to write to leave that state, and say in the comment why
  sampling was rejected, with both numbers.
- **A probe that reads at two points gives an upper bound on a gap, never the gap.** Re-probe at a
  finer rate before a number from it goes into a comment.

## Evidence

`TestClusteredNatsHubCluster` (`packages/envoy/infra/gcp/clusteredhub_live_test.go`, build tag
`natslive`) kills the server that leads both the `LIVE_R3` stream and JetStream's meta group, then
waits for both survivors to name one live meta leader before it reads the stream. Its control that
the leaderless window really happened is the new meta leader's own
`Self is new JetStream cluster metadata leader` line.

The first round asserted that control with `require.Greater` on the count **after** the wait on
both survivors' `/jsz`. In nats-server v2.15.1 `/jsz` names the leader from `n.GroupLeader()`
(`server/jetstream_cluster.go:14075`), set by `switchToLeader`'s `n.updateLeader(n.id)`
(`server/raft.go:6307`) and taken by a follower from the first append entry (`:4839-4841`), while
the line is written only once the leader's first entry as leader is applied (`server/raft.go:6321`,
`1603-1613`, then `server/jetstream_cluster.go:10080`). So the endpoint leads the log, and the
single read could miss it.

Measured: a reviewer's probe polling every 1 ms saw both survivors agree with no line in the log
in 1 of 15 runs; a tester's microsecond-rate probe measured the gap at 2.776, 11.177, 11.325 and
13.739 ms in 4 of 12 runs, and at 6-48 µs in the other 8. The first comment wrote the gap as
"20 ms later in one run of fifteen", which was the two-point probe's upper bound rather than the
gap, and was corrected to the measured figures in commit 9b07bed67f3c.

The fix (commit 80bc419e9d9b) moved the count into the `within` that waits for the agreement, as
its last conjunct, and deleted the assertion after it. `within` ends the test with `t.Fatalf` on
timeout (`clusteredhub_live_test.go:671`), so the wait cannot return without the line and no second
read is needed. Twenty of twenty runs passed at the fixed head, and again at the tester's. With
`selfElected` set to a line nats-server never writes — the `/jsz` agreement alone would then end
the wait — the run was refused with
`a new JetStream meta leader after the SIGKILL, with its own election in its log: not within 30s: envoy-nats-2, which the survivors name the new JetStream meta leader, has not logged "…" since the kill`,
which is the wait's name carrying the missing fact.

The issue's own acceptance first asked the test to sample the leaderless state directly
(`meta_cluster.leader == ""` at least once). It was rejected and renegotiated at plan time: that
state lasted 18, 20, 22, 43 and 49 ms in five of six measured runs, and `within` polls every 100 ms
(`clusteredhub_live_test.go:673`), so the sample would have been a coin flip in a test being fixed
for failing at random. The log line serves instead because a raft group reaches a new leader only
through a term that has none.
