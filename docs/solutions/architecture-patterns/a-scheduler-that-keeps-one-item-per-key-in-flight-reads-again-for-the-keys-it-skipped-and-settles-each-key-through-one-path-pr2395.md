---
title: "A scheduler that keeps one item per key in flight reads again for the keys it skipped, and settles each key through one path"
category: architecture-patterns
tags:
  - per-key-order
  - in-flight
  - wake
  - no-poll
  - settle
  - outbox
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/outbox
related_issues:
  - "sjawhar/legion#2395"
---

# A scheduler that keeps one item per key in flight reads again for the keys it skipped, and settles each key through one path

Applies to any loop that runs batches side by side and keeps each key (an owner, a session, a
tenant) in one batch at a time by leaving the keys in flight out of its read
(`where key <> all($in_flight)`), most of all one woken by events with no periodic poll.

- **Report the exclusion from the read.** The function that reads while keys are in flight
  returns whether it left any out. It need not prove it skipped work: "some key was in flight"
  is enough.
- **Read again once those keys are back.** A wake that arrives while its key is in flight is
  spent on a read that cannot see that key's rows. When a batch that read with keys left out
  finishes and nothing is in flight, read once more instead of waiting for the next wake. That
  read leaves nothing out, so it cannot set the flag again and the loop cannot spin.
- **Settle each key through one function.** When a key can settle early (as soon as its own
  sends finish) and at the batch's end, both call the same settle-and-record function. Never
  keep two copies of it.
- **The early path returns without settling every case only the end can decide.** Name those
  cases in one condition (in the outbox: a send that lost its connection, a send fenced by the
  lease deadline, a send never made, a context ended, a deadline passed), and mark a key settled
  only once that function has run for it. Otherwise one attempt is classified, counted, logged
  and written twice.

## Evidence

- `outbox/lease.go:564-582` (`lanes.reserve` returns `excluded = len(inFlight) > 0`),
  `lease.go:519` (`missed = missed || batch.excluded`) and `lease.go:470-479` (the re-read
  when `inflight == 0`, with why it cannot loop). Commit `da5762a8`.
  `TestAnEventCommittedWhileItsOwnerIsInFlightIsReadWhenThatOwnerIsBack`
  (`outbox/owner_lane_test.go:148`) failed at `9cabeaf6`: the event was not sent within 8 s of
  its commit, because it waited for a back-off timer.
- `outbox/publisher.go:264-271` (`settleAndRecord`, the one path), `:305-316` (`settleOwner`
  and its bail condition) and `:387` (the batch end settles what no owner did). Commit
  `da5762a8`. `TestAnOwnerLeftForTheBatchsEndLogsItsFailureOnce`
  (`outbox/owner_lane_test.go:232`) logged two publish lines for one attempt at `9cabeaf6`.
- Both were found in review after the per-key lanes went in (commit `767794be`), whose own test
  `TestAHolderThatNeverAnswersDoesNotDelayAnotherIssuesEvents`
  (`outbox/stalled_holder_latency_test.go:23`) pinned the latency and not the liveness.
- Nearest documents: `docs/solutions/database-issues/a-cross-row-one-at-a-time-rule-in-a-where-predicate-is-write-skew-under-read-committed-lock-the-key-first-LEGION-694.md`
  enforces the exclusion and says nothing of what wakes the skipped work;
  `docs/solutions/architecture-patterns/turning-a-full-scan-loop-into-a-change-feed-and-a-deadline-queue.md`
  covers a mark the writer misses, not a reader that leaves busy keys out.
