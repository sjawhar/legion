---
title: "A due-only read puts its window in the key, and pays separately for what the scan also did"
category: architecture-patterns
tags:
  - nats
  - jetstream
  - key-value
  - change-feed
  - index
  - benchmarks
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/redeliver
related_issues:
  - "sjawhar/legion#2641"
---

# A due-only read puts its window in the key, and pays separately for what the scan also did

Extends docs/solutions/architecture-patterns/turning-a-full-scan-loop-into-a-change-feed-and-a-deadline-queue.md,
which covers a loop whose source is a local cache it can filter in memory. This one is about a loop
whose source is a remote store that selects only by key, where the window has to be in the key, and
about what the removed scan was quietly also doing.

## Rules

- **A store that selects only by key or subject cannot make the read's cost independent of entries
  that are open and not due while the due time sits in the value.** A spec that says "index the
  pending entries as `pending.<GUID>` with the due time inside" still streams one message per open
  entry. Put the due time in the key, ordered coarse to fine (`pending.<YYYYMMDD>.<HH>.<MM>.<GUID>`),
  and read the window as a set of **disjoint** filters over that decomposition — whole days, then
  whole hours of the part-day, then minutes — so no entry is delivered twice and nothing outside the
  window is delivered at all. Changing the key shape is a departure from the spec's text, so record
  it with its reason rather than quietly shipping another shape.
- **Decompose the window in one pure function with its own unit test for coverage and
  disjointness.** The decomposition is where an off-by-one costs either a missed due entry or a
  duplicate, and neither is visible in the loop's own output.
- **The margin that decides "this entry is a stray" must carry the key's truncation, not just the
  cursor's overlap.** An entry keyed to the minute of a moment at or after its write says only that
  the write was before that minute's end, so the margin is the cursor overlap **plus** one minute.
  Derive it from the cursor constant (`strayMargin = cursorOverlap + time.Minute`) rather than
  writing a second number.
- **Before you delete the scan, list everything the scan was also doing.** Here it was the repair:
  it reached a record whose index entry was never written, and a record an older build wrote with no
  entry at all. Each of those needs a named trigger with a bound of its own, and "the next scan will
  get it" is no longer an answer. Write each trigger and what ends it next to the key's own comment,
  and keep that comment true — a reviewer found it claiming that both triggers end the same way when
  one ends only on a complete sweep of this build.
- **An index is a second, non-atomic write, so the loop's progress marker must not pass an entry
  whose write did not land.** A record the sweep could not index leaves the pass incomplete, the
  cursor does not advance past it, and the bucket is not marked back-filled. A write that *lost* its
  compare-and-set carries no error at all, so the helper has to report "not written" separately from
  "failed" (see
  docs/solutions/envoy/a-key-value-write-has-three-outcomes-and-a-caller-that-conflates-them-drops-the-record-pr2641.md).
- **State the performance claim in the resource you bounded, and measure it on the real server
  across a socket.** Client-visible traffic here is flat in the number of open-not-due entries
  (6 messages / 2.8 kB per sweep at 10,000, `/varz` sampled either side of one sweep, with
  `index_entries=0`); server-side work for a multi-filter consumer is not, because it scales with the
  stream's subject cardinality rather than with what matches. Say which of the two the benchmark's
  ratio criterion measured, and why it is unmet, instead of restating the flat number.

## Evidence

- Key and window: `packages/envoy/internal/dispatch/redeliver/index.go`, `pendingKey` (:45),
  `parsePendingKey` (:52), the filter decomposition and its disjointness unit test; the margin at
  `index.go:141-147`, derived from `cursorOverlap` (`sweep.go:84-89`).
- The read window's own bound: `Lookback = maxBackoff + 3*Interval` (`sweep.go:69-78`) — a record has
  to be read while GitHub's answer to its last request is still in the listing, so the window outlasts
  the longest backoff plus the sweeps it takes for a due entry to be read. The regression test fails
  against the old constant.
- The repair and its triggers: `backfillIndex` (`index.go:283-339`), `indexFresh` (`:341-359`).
- The decomposition: `pendingFilters` (`index.go:112`), read by `openDue`'s `WatchFiltered`;
  `TestPendingFiltersCoverTheWindowWithoutOverlapping` (`index_test.go:22`) is the coverage and
  pairwise-disjointness test.
- Incompleteness: `backfillIndex`'s `indexed = false` paths (`index.go:311-326`), which leave the
  marker unwritten so the next sweep reads every record again.
- Measurement: pull request #2641's `E2E (implementer)` line, E-2, `/varz` either side of one sweep
  over 10,000 open deliveries, base `0642a37abdaa` as the control.
