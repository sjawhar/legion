---
title: "A batch query's failure belongs to the events that needed it, not to the batch"
category: envoy
tags:
  - dispatch-outbox
  - batching
  - error-handling
  - ordering
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/outbox
---

# A batch query's failure belongs to the events that needed it, not to the batch

When you add a query that a batched publisher (the Dispatch outbox, a fan-out) runs once for a
whole batch before it sends anything, follow these rules:

- **Carry the error on each event that needed the query, not as the row's failure.** A row the
  outbox marks failed is skipped before it sends anything, including its owner topic. That event
  then lands on the issue's subject after later events of the same issue, and a consumer that
  applies events by sequence drops it as stale.
- **Send what does not depend on the query first.** Send the owner topic, the route and the
  claimant, then end the attempt with the query's error at the step that needed the query. Record
  the destinations the attempt did reach, so a retry sends only the rest.
- **Let settle decide between a back-off and a release.** A query that fails because the process
  is stopping fails with `context canceled`. Treated as the event's own failure, that adds an
  attempt, a retry delay and an error log to every event of a batch that was only interrupted. Left
  to the attempt, the outbox's existing `ctx.Err()` check releases those events with no attempt
  counted.
- **Test both ways the query can fail.** Use a store error (stored data the query refuses, such as
  a JSON scalar where it expects an array) and a cancelled context. Assert the recorded
  `published_destinations`, the attempt count and the retry time. A non-dependent event in the same
  batch must publish normally.

## Evidence

sjawhar/legion#2366 first marked every reply in a batch failed when its recipient query failed.
Review found that the owner topic was dropped and that a stopping batch was backed off rather than
released. Commit fbbd1712 added `recipientList{sessions, err}` (`outbox/recipients.go`) and the two
tests in `outbox/publisher_comment_recipients_test.go`
(`TestAFailedRecipientQueryBacksOffOnlyTheEventsThatNeededItAfterTheirOwnerTopic` and
`TestAStoppingBatchReleasesTheEventsWhoseRecipientQueryFailed`), which fail on the earlier code.
