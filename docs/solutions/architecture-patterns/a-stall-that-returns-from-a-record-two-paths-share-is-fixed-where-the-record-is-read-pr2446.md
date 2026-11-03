---
title: "A stall that returns from a record two paths share is fixed where the record is read"
category: architecture-patterns
tags:
  - shared-record
  - cache
  - delivery
  - review-rounds
  - envoy
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2446"
---

# A stall that returns from a record two paths share is fixed where the record is read

- When one path writes a record and another path reads it as a gate (an attempt cache, a lease, a
  marker), and the same stall comes back from a new writer state after you fixed one, stop fixing
  writer states one at a time. Make the reader compute its verdict from the shared inputs first,
  as if no record stood, and consult the record only for the one meaning it can act on.
- That works only where the reader can compute the verdict from the same inputs the writer read.
  Keep every writer-side invariant the reader relies on, and state it at both ends (for example,
  "a refusal is recorded only together with the exception that reports it").
- A record that means two things ("a push in flight" and "a refusal already reported") causes the
  next stall. When you can change its shape, tag the record with its meaning, so the reader never
  has to infer it.
- Where two paths can each report the same outcome, write out the (recipient state × verdict)
  matrix. Give every cell exactly one reporter, and prove each cell with a test, before review
  finds the missing cells for you.
- Lock the fix with one test that drives every writer state the record can come from, not one test
  per state.

## Evidence

- In sjawhar/legion#2446 the durable path's fan-out wrote an attempt record, and the serving-port
  relay of a session's JetStream consumer held a message under it for `attemptCacheTTL`, 5 minutes.
  Review rounds 5, 6 and 7 each removed one entry state in which the fan-out wrote a refusal
  record (`196233c0`, `0debf189`, `bafcc923`). Round 8 found a fourth: a registration tick that
  fell back to core delivery for a moment.
- Round 8's fix (`a4b31595`) split the judgement into `judgeFollowedTopicWithoutAttempt`
  (`packages/envoy/cmd/listener/delivery.go:878`), which both paths apply, and
  `judgeFollowedTopic` (`:859`), which adds the attempt cache for the fan-out alone. The relay
  (`sessionRelays.followedTopicVerdict`, `session_relay.go:946`) judges without the record, acks a
  refusal the fan-out reported, and holds only a message it would push
  (`holdWhilePushedElsewhere`, `:1009`). It relies on the writer's invariant in
  `fanoutRefusalOutcome` (`delivery.go:914`).
- `TestARefusedMessageNeverHoldsTheRelayWhateverTheEntrySaid`
  (`cmd/listener/session_relay_refusal_states_test.go`) drives all four entry states, and fails on
  the code before the fix. The one-reporter rule reads as one predicate,
  `relayJudges := relayed && target.Port > 0` (`delivery.go:999`). The cell where nobody reported
  (round 6) and the cell where two paths reported (round 9) were found by review, not by tests.
  After that, the PR published the matrix each round.
- What stays open: the relay still infers "a record means already reported". Round 9 recorded the
  case of a refusal next to a record from a real push, which a tagged record would close.
