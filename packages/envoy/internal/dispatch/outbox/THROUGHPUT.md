# Outbox throughput readings

Readings taken 2026-10-26/27 on a shared development machine under load from other test runs.
Treat the numbers as order-of-magnitude figures.

## Method

`TestOutboxThroughputUnderRemoteLatency` (`throughput_test.go`):

- A real Postgres (pgvector pg16) and a real JetStream server.
- The outbox's bus client reaches NATS through a TCP proxy that holds each chunk for half the
  round trip, in order (`startLatencyProxy`), as a remote NATS hub does.
- Load: 200 events across 20 issues, each routed to `role:reviewer`. Half are human replies to a
  session's message, which take an author lookup and a third destination. That is 500
  destinations in all: 200 issue topics, 200 role-lane publishes and 100 session topics.
- The "busy pool" case keeps a full shared pool busy. The table's readings ran 32 goroutines of
  `select pg_sleep(0.01)` back to back on the 16-connection shared pool production then had.
  Since the shared (request) pool holds 48, the test fills a shared pool of 8 of its own with 16
  such goroutines (`busySharedPool`), so it does not hold 48 server connections while other
  packages' tests share the server; on 2026-10-27 that read 1,050-1,260 ev/s at 50 ms and 517-566
  at 150 ms.
- The rate is (events marked published) / (time from start until none is left).

## Results

| Design | 50 ms idle | 50 ms busy | 150 ms idle | 150 ms busy |
|---|---|---|---|---|
| Old (`origin/main` at 1357b95c2, one synchronous publish and one write per destination) | 2.7 ev/s (5.3 run alone) | 3.0 ev/s | 1.7 ev/s | 1.9 ev/s |
| New (one window per batch, one outcome write, outbox pool of its own) | 1,100-1,340 ev/s | 1,100-1,230 ev/s | 540-570 ev/s | 480-540 ev/s |

The test requires 25 ev/s. The old design fails it in every case.

## Comment replies to the claimant and the thread (2026-10-27, sjawhar/legion#2366)

This change, as measured, sent a person's comment on an issue with no route to the session that
claimed the issue. When no session claimed the issue, it sends the comment to every session that
wrote the issue's primary document. It also sends a reply to every session in its thread and to
every session that wrote the thread's document. For the whole batch, it reads the threads'
sessions in one query (`loadCommentRecipients`) and the primary documents' sessions in one query
(`loadPrimaryDocumentAuthors`).

Since the merge with #2225, the claimant's copy comes from main's claimant route
(`publishClaimantRoute`, the claim read in the scan), on a routed issue too; no load here has a
claimed routed issue, so each load's stream count is unchanged and the runs below were not
repeated.

Method: the test has three loads. Each has 200 events across 20 issues, so each batch of 100 has
every kind of event in its load.

- `messages` (`seedThroughputBacklog`): the load of the Method section above.
- `comment replies` (`seedCommentReplyBacklog`): half are a person's replies in a comment thread,
  on 10 claimed issues with no route. Each thread is on a passage of the issue's document and
  holds the person's root comment and a reply from the session that wrote the document. The
  other half are messages on 10 issues routed to `role:reviewer`, as in the `messages` load.
- `unclaimed comments` (`seedUnclaimedCommentBacklog`): all are a person's comments on 20 issues
  with no route and no claim. A session wrote the primary document of each issue. Half are
  comments on the issue's conversation. The other half are replies in a thread on a passage of
  the primary document, which holds the person's root comment and a reply from a second session.
  Every batch runs `loadPrimaryDocumentAuthors` and `loadCommentRecipients`.

The trees and the runs:

- Main before #2225: `origin/main` at 1b8d5c13e, whose outbox package was the same as at this
  branch's merge base then (c3d09eec9); its `internal/bus` package had changes that head did not
  have. Head: c2a223fcb, before the merge with #2225. Main ran head's `throughput_test.go` in an
  exported copy of its tree. The rows marked "main" are that tree, not main as it is now.
- Postgres 17, 2 CPUs. Three runs of each tree, main and head in turn, back to back. Each run
  ran all three loads.
- Each cell is the range of the three runs, in events per second.

| Load | Tree | 50 ms idle | 50 ms busy | 150 ms idle | 150 ms busy | Destinations per event |
|---|---|---|---|---|---|---|
| `messages` | main | 445-693 | 550-610 | 420-437 | 366-386 | 2.5 |
| `messages` | head | 492-764 | 363-693 | 211-436 | 288-345 | 2.5 |
| `comment replies` | main | 391-572 | 337-394 | 324-379 | 315-332 | 1.75 (reply 1, message 2.5) |
| `comment replies` | head | 707-793 | 615-656 | 357-434 | 354-381 | 2.75 (reply 3, message 2.5) |
| `unclaimed comments` | main | 620-630 | 302-559 | 381-391 | 291-363 | 1 (comment 1, reply 1) |
| `unclaimed comments` | head | 676-856 | 604-682 | 438-447 | 305-383 | 2.5 (comment 2, reply 3) |

On main before #2225, a person's comment on an issue with no route reached only its issue topic.
It did not reach the claimant, the primary document's writer or the thread's sessions, so that
tree failed the test's count of stream messages: 250 of 450 for `comment replies` and 200 of 500
for `unclaimed comments`. It passed the 25 ev/s requirement in every case. Main now has #2225's
claimant route, so in the `comment replies` load today's main reaches the claimant too; the
claimant copy is main's, and what this change adds is the thread's sessions and the documents'
writers.

The `messages` load at 150 ms was lower on head in these three runs. Five more runs of that case
alone, main and head in turn, read 395-431 (idle) and 286-387 (busy) ev/s on main and 306-430
(idle) and 297-371 (busy) on head, so the difference is in the spread of the runs.

Judgement: against main before #2225, the change cost no measurable throughput: both comment loads
were as fast or faster on head, although each comment reached two or three destinations in place
of one, and the `messages` load was the same within the spread of the runs. Not re-measured
against main with #2225.

## Order and crash safety

| Test | Old design | New design |
|---|---|---|
| `TestABacklogPublishedAcrossANATSOutageKeepsItsOrder` (NATS down before the scan, then back) | Fails: an issue's subject held `[3 5 7 … 19 1]`, because event 1 was backed off | Passes: in id order, 0.5-0.7 s after NATS came back |
| `TestABatchCutOffInFlightKeepsItsOrder` (connection cut with acknowledgements in flight at 150 ms round trip) | Fails on the first pipelined build: 100 events backed off, then reordered | Passes: every event released, no attempt counted, stream in id order with no duplicates |
| `TestAnOutboxThatDiesBetweenAckAndMarkPublishesAgainWithoutDuplicates` | Passes | Passes: republished once the claim lapses, the stream's MsgId drops every repeat |
| `TestABatchIsSentBeforeAnyAnswerAndMarkedOnlyOnceEveryAnswerIsIn` | n/a | Passes: nothing is marked or backed off until every answer is in |
| `TestAnOutboxStoppedMidBatchMarksTheAnsweredAndReleasesTheRest` | n/a | Passes; fails if the outcome write uses the cancelled context |

## What order consumers rely on

- The Legion daemon applies an issue's facts by their per-issue `seq` and drops one at or below the
  last it applied (`daemon/internal/workflow/engine.go`, `admit/admit.go`). So on the success
  path each issue's events must reach its subject in id order. A late older event is lost. One
  issue's ids commit in id order, since the broker draws them under the issue's row lock; two
  issues' ids do not, and no consumer of NATS relies on an order across issues
  (`TestOneIssuesEventsReachItsSubjectInSeqOrderWhileAnotherIssueCommitsAcrossThem`).
- The Envoy listener dedupes by `dedupe_key` and relies on no order (`cmd/listener/delivery.go`).
- The SSE stream reads Postgres, not NATS.

## Known limits, not addressed here

- Two processes reorder an issue's events even on the success path. Process A claims ids 1-100
  and process B claims 101-200 at the same time. One publisher process at a time (a lease) would
  close this.
- Wakes are not coalesced. Each committed event runs its own scan, and a caught-up outbox pays the
  claim and the read per event.
- A failed outcome write resends the whole batch. JetStream drops the repeats by MsgId; role
  lanes carry them, and their receivers drop them by dedupe key.
