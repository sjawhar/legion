---
title: "A fan-out copy defers to a direct send only once that send was recorded delivered, and the wait is bounded by the sender's own lease"
category: envoy
tags:
  - dispatch-outbox
  - mentions
  - delivery
  - deduplication
  - deferral
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/outbox
---

# A fan-out copy defers to a direct send only once that send was recorded delivered, and the wait is bounded by the sender's own lease

When the outbox skips a session's own-topic copy because another path delivers the event to that
session (a comment's mention, sent through the Envoy listener), apply these rules:

- **Skip only on a recorded delivery, never on a resolution.** That a mention resolved to a session
  when the comment was written names a candidate, not a delivery: the send runs after the commit
  and can fail (listener 404, receipt timeout, cancelled handler, a mode the session does not
  advertise). Read the delivery record when the event is published and skip only for a `sent`
  attempt. A failed, abandoned or unrecorded send gets the outbox copy.
- **Send every path through one predicate.** Every copy that can overlap the direct send (route,
  claimant, author, follower) asks the same function. One path left on the old rule is the bug.
- **Hold an undecided copy back, bounded by the sender's own lease, taken from one constant both
  sides import.** While the send is still pending, publish every other destination and defer the
  event a few seconds. Count it pending only while its claim, or its creation when nobody claimed
  it, is younger than the lease the sender's own retry uses. A deferral counts no attempt, which is
  safe only because that bound ends it.
- **Let a deferral win only when it is the whole outcome.** Compare the deferral sentinel by
  identity, after denials, failures and lost connections have been ruled out. `errors.Is` also
  matches it inside a joined error, and then a denied destination retries every few seconds and
  never reaches its dead-letter count. Lock it with a deferral plus a denial (backed off, attempt
  counted) and a deferral plus a lost connection (released). Aim the negative control at the code
  that actually sets the order, or it passes for the wrong reason.
- **Write down what the deferral costs where it is defined, and in every agent-facing text that
  promises one copy.** A held creation can arrive after the same comment's edit. A receipt timeout
  is recorded `failed` though the send may have landed, so the session gets two copies under
  different dedupe keys.

## Evidence

- In `outbox/publisher.go`, `attempt.leftToMention` is the one predicate. It reads
  `mentionDeliveryQuery` (`comment_deliveries`, grouped by session, `coalesce(claimed_at,
  created_at)` against `model.DeliveryClaimLapseInterval`), and the route, claimant, author and
  follower copies all call it. `model.DeliveryClaimLapse` also builds the API's `claimLapsed`
  (`api/envoy_resolve.go`). `settle` defers on `err == errMentionPending` only.
- Before commit 3befbe7e, the claimant and follower copies were skipped on `mentions[].session_id`
  (resolution). A failed mention then left the claimant with no copy, and it removed the follower
  copy that base had always sent. The live cases `mention-failed-claimant` and
  `mention-failed-follower` in `scripts/e2e/dispatch-claimant-delivery.sh` fail on that build and
  pass after it.
- `TestAHeldBackClaimantCopyDefersNoAttemptThatFailedElsewhere` and
  `TestAScanDefersAClaimantCopyWhileItsMentionIsBeingSent` (`publisher_mentions_test.go`) pin the
  outcome table. The implementer's first mutation reordered `settle` and the tests still passed,
  because `attempt.err` already orders failures before the sentinel. The tester's mutation of
  `attempt.err` was the control that failed (PR comment 6449381816).
