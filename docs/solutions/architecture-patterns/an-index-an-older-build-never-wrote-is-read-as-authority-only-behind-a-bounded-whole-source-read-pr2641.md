---
title: "An index an older build never wrote is read as authority only behind a bounded whole-source read"
category: architecture-patterns
tags:
  - rolling-deploy
  - index
  - nats
  - key-value
  - jetstream
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/redeliver
related_issues:
  - "sjawhar/legion#2641"
---

# An index an older build never wrote is read as authority only behind a bounded whole-source read

Extends docs/solutions/architecture-patterns/a-new-kind-of-index-row-ships-with-no-producer-when-every-build-reconciles-the-index-wholesale-pr2394.md,
which is about an older build *deleting* rows of a kind it does not know. This one is about an older
build writing **no entry at all**, while the new build's reader has stopped reading the source
whole. Five of the six implementing rounds of #2641 were spent on one defect class under this
heading, and review found every one of them.

## Rules

- **Review it as a matrix, not as a timeline.** For each path, ask which build wrote the entry and
  which build reads it, and name the four cells. The cell that costs a delivery is "written by a
  build with no index, read by a build that trusts the index": every entry absent, and the reader's
  own emptiness indistinguishable from "nothing is due". A rollback is the same cell with the builds
  swapped, and it can outlast the data's retention.
- **A reader whose index may be incomplete needs a bounded whole-source read, and each of its
  triggers needs a stated end.** Here: no marker at all, or a marker older than the read window, runs
  the source whole again; a cursor an older build left runs it on every sweep until a complete sweep
  of this build writes a cursor of its own. Write both triggers and both ends where the key is
  defined, and bound the cost: one whole-source read an hour at most for as long as the repairs
  land.
- **The flag that says "this build's work landed" carries a time and decodes to something a reader
  can judge.** A bare marker key says only "someone marked it once", which is useless after a
  rollback; a marker holding the moment the repair worked from lets the reader ask "recent enough".
  A value that does not decode — the empty value an earlier build of the same change wrote — says
  nothing about when its repair ran, so read it as a marker to replace, never as an error.
- **Absence is not a disowning.** An entry whose record names no entry is adopted by that entry, not
  deleted by it: only a record naming a *different* entry proves this one stale. Without that rule,
  the reader deletes exactly the entries an older build's records left unnamed and the delivery is
  never asked for again. The cleanup for the duplicates adoption allows is the margin (a stray that
  has fallen behind), not the reader's own key.
- **The repair never takes a record away from a live claimant.** A record another process holds in
  flight gets its index entry written at the claim's end and no record write, since the record write
  would take the claimant's revision from it. State which clock each side reads and in which
  direction they may disagree: the repair reads the sweep's own start, at or before the deciding
  path's later reading, so a claim the decider treats as live is live for the repair too — the safe
  direction.

## Evidence

- `packages/envoy/internal/dispatch/redeliver/index.go`: `backfillIndex`'s `olderBuild` parameter
  (:283-295), the live-claim branch and its clock note (:305-317), `indexFresh` reading an
  undecodable value as stale (:341-359), `indexClaimEnd` (:380).
- Adoption: `openDue`'s `revision == 0 || !current.open() || (current.Pending != "" &&
  current.Pending != key)` and the comment under it (`index.go:208-222`).
- The trigger ends: the `indexKey` comment (`index.go:235-244`), corrected in review round 5 after it
  claimed both triggers end when a back-fill lands.
- Tests: `TestAnIndexEntryWhoseRecordNamesNoneIsAdopted`,
  `TestABackfillLeavesALiveClaimToItsClaimant`,
  `TestAMarkerOlderThanTheLookbackIsBackfilledAgain`,
  `TestAStrayIndexEntryIsDeletedOnlyPastTheMargin` (`redeliver/sweep_test.go`), each verified red
  against the reviewed code in a scratch copy.
