---
title: "End a successor's safety hold on a mark its predecessor writes after it finished, never on elapsed time"
category: architecture-patterns
tags:
  - hand-over
  - lease
  - nats-kv
  - jetstream
  - take-over
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2404"
---

# End a successor's safety hold on a mark its predecessor writes after it finished, never on elapsed time

## The rule

1. When a new owner holds back work after a take-over (a settle hold, a fence, a grace period) and
   a clean hand-over could let it hold less, make the short path depend on an explicit mark. The
   old owner writes it, and only after the last step it vouches for has finished. Do not infer a
   clean hand-over from timing: how long ago the release was, or whether the new owner subscribed
   inside some window. Each timing rule leaves another path where the vouched-for step did not
   happen, and each fix finds the next one.
2. Make the short path an addition to a long path that is sound alone. The long bound (here, one
   ack wait and a margin) holds on every path with no mark. A mark that is missing, unreadable,
   failed to write, or names another hand-over must leave the long bound in force. Losing a mark
   then costs only time.
3. Check each condition of a clean hand-over in one function, in order, and give each a named
   reason for writing no mark. Log the reason, and have each no-mark test row assert which reason
   stopped the mark, not only that none was written.
4. The new owner must wait for the mark; it must not decide at its own start. When the old owner
   lingers until the new one is ready, the mark lands after the new owner started. So read it on
   every poll of the hold, and end the hold early only once it is there.
5. Never let the old owner write into the record the new owner took: its revision has changed and
   the write would fail or overwrite the new state. Put the mark in a key of its own, named by
   the revision the old owner itself released, and let the bucket's age limit remove it. Clear
   what the owner remembers of its own last release at the start of each term, so a mark vouches
   only for this term's release. A release that timed out vouches for nothing.

## Evidence

- `packages/envoy/cmd/listener/receive_stop.go:162` (`markHandBack`): the gates, checked in order
  (linger ended with the next holder receiving, drain confirmed, handler returned, then the lease's
  own check), each with a `noMark*` reason. `lease_handback.go:54` (`MarkHandBack`) writes
  `handback.<durable>.<revision>` for the revision this term's own release wrote, and `:79`
  (`HandBackMarked`) is the one read. `receive_term.go:374` (`readHandBackMark`) returns false on a
  read error, so the hold runs to its 7 s cap.
- Review rounds 4 to 6 of #2404 each closed one timing path of the inferred hand-back: the linger's
  `timeout` exit, a slow or failed subscribe flush, and a late release after a `Release` that timed
  out. Each time the next path still gave a 3 s hold with nothing bridging to it. The explicit mark
  (commit eafa3120) closed the class. Rows that failed first:
  `TestASuccessorWhoseSubscribeTheServerNeverGotSettlesForTheAckWait`
  (`receive_stop_test.go:600`) and `TestLeaseAReleaseThatTimedOutLeavesTheTermOwingTheAckWait`
  (`lease_test.go:423`).
- `TestAStopMarksNoHandBackWhenItsLingerOrItsTermFellShort` (`receive_stop_test.go:350`) checks
  `no_mark` for each gate. Before that column, the stale-binding row would have passed with the
  drain gate firing instead (round 8).
- The fast path costs nothing in a clean stop. `TestTwoPodsGracefulStopWithARetryOutstandingDeliversWithin10s`
  (`multiprocess_handback_test.go:282`, real binaries): the slowest of 60 messages arrived in
  3.315 s with the mark, and in 7.21 s with the mark write turned off.
