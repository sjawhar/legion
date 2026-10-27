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
  Since the shared pool holds 64, the test fills a shared pool of 8 of its own with 16 such
  goroutines (`busySharedPool`), so it does not hold 64 server connections while other packages'
  tests share the server; on 2026-10-27 that read 1,050-1,260 ev/s at 50 ms and 517-566 at 150 ms.
- The rate is (events marked published) / (time from start until none is left).

## Results

| Design | 50 ms idle | 50 ms busy | 150 ms idle | 150 ms busy |
|---|---|---|---|---|
| Old (`origin/main` at 1357b95c2, one synchronous publish and one write per destination) | 2.7 ev/s (5.3 run alone) | 3.0 ev/s | 1.7 ev/s | 1.9 ev/s |
| New (one window per batch, one outcome write, outbox pool of its own) | 1,100-1,340 ev/s | 1,100-1,230 ev/s | 540-570 ev/s | 480-540 ev/s |

The test requires 25 ev/s. The old design fails it in every case.

## Comment replies to the claimant and the thread (2026-10-27, INFRA-596)

INFRA-596 sends a person's comment to the session that claimed an unrouted issue. It also sends
a reply to every session in its thread and to every session that wrote the thread's document.
The outbox reads the claimant in its scan, and reads the thread's sessions for the whole batch in
one query.

Method: the test has a second load, `comment replies` (`seedCommentReplyBacklog`). It has 200
events across 20 issues. Half are a person's replies in a comment thread, on 10 claimed issues
with no route. Each thread is on a passage of the issue's document and holds the person's root
comment and a reply from the session that wrote the document. The other half are messages on 10
issues routed to `role:reviewer`, as in the `messages` load.

- Main: `origin/main` at 448a41003 (the outbox package is the same as at the merge base
  c3d09eec9). Head: 4c3eb5caf. Main ran this head's `throughput_test.go` in an exported copy of
  its tree.
- Postgres 17, 2 CPUs. Three runs of each tree, main and head in turn, back to back.
- Each cell is the range of the three runs, in events per second.

| Load | Tree | 50 ms idle | 50 ms busy | 150 ms idle | 150 ms busy | Destinations per event |
|---|---|---|---|---|---|---|
| `messages` | main | 646-741 | 442-562 | 379-424 | 339-402 | 2.5 |
| `messages` | head | 626-723 | 483-549 | 375-428 | 320-373 | 2.5 |
| `comment replies` | main | 454-561 | 255-412 | 189-374 | 172-327 | 1.75 (comment 1, message 2.5) |
| `comment replies` | head | 674-689 | 500-636 | 423-440 | 333-386 | 2.75 (comment 3, message 2.5) |

On main, a comment reply reaches only its issue topic. It does not reach the claimant or the
document's writer, so main fails the test's count of stream messages (250 of 450). Main passes the
25 ev/s requirement in every case.

Judgement: the change costs no measurable throughput. The `messages` load is the same within the
spread of the runs. The `comment replies` load is faster on head, although each reply reaches
three destinations in place of one.

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
  path each issue's events must reach its subject in id order. A late older event is lost.
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
