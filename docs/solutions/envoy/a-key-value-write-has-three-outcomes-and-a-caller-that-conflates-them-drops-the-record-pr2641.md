---
title: "A key-value write has three outcomes, and a caller that conflates them drops the record"
category: envoy
tags:
  - nats
  - jetstream
  - key-value
  - compare-and-set
  - error-handling
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/redeliver
related_issues:
  - "sjawhar/legion#2641"
---

# A key-value write has three outcomes, and a caller that conflates them drops the record

Extends docs/solutions/envoy/a-jetstream-answer-is-not-the-outcome-a-sequence-zero-duplicate-stored-nothing-and-a-timed-out-update-may-have-applied-pr2362.md,
which is about an answer that says "stored" and stored nothing. This one is about the two kinds of
"not stored" a compare-and-set write returns, and what each owes its caller.

## Rules

- **Three outcomes, three answers: applied, lost the race, refused.** A revisioned `Put` returns
  `(revision, written bool, error)` so each caller gets all three. A write that **lost** its
  compare-and-set carries no error: another writer decided this key first, and what that writer
  wrote carries the work on, so the loser may pass. A write the store **refused** carries its error
  and carries nothing on at all: the key still holds what it held, and the caller is the only thing
  standing between that and a lost record. Answering a refusal as "someone else got there first" is
  how a record is read once, never written, and never asked for again.
- **One sentinel for "the store refused", and the message says which half it was.** A helper that
  writes a record and its index entry together wraps each refusal as
  `fmt.Errorf("%w: record of %s: %w", errNotWritten, guid, err)` and
  `"%w: index entry of %s: %w"`, so the one sentinel the loop matches tells the caller the write
  did not land and the line tells a reader which write it was. One sentinel per half is two
  identifiers where no code ever matches the second: review round 2 had them removed as dead.
- **Classify a lost race by the typed error *and* the server's text.** nats.go answers a wrong
  expected revision as `nats.ErrKeyExists` in some paths and with a message containing
  `wrong last sequence` in others, so keep both in one `isConflict` function and call it from
  nowhere else.
- **A write refused inside a scan must stop the scan's progress marker, not just its own key.** The
  caller that sees `written == false` with an error leaves the pass incomplete, so the cursor does
  not pass the key and no marker says the pass covered the bucket.

## Evidence

- `packages/envoy/internal/dispatch/redeliver/state.go:235-256`: `putRecord`'s three-way switch and
  its doc, `isConflict` matching `nats.ErrKeyExists` or `wrong last sequence`.
- `writeRecord` (`index.go:413`) pairs the record write with the index entry and returns the same
  triple to its callers; `errNotWritten` (`index.go:387-391`), wrapped at `index.go:426` and `:447`
  and matched once, at `sweep.go:426`.
- Tests: `TestARefusedRecordWriteLeavesTheDeliveryForTheNextSweep` (`sweep_test.go:1475`),
  `TestABackfillWhoseRecordWriteIsRefusedLeavesTheIndexUnmarked` (:1758) and
  `TestABackfillThatLosesItsRecordRaceLeavesTheIndexUnmarked` (:1813) — the two latter are the pair
  that makes the refusal and the lost race different cases. Each was verified red against the
  reviewed code.
- The defect this closed, found by review round 1: `decide` answered every failed write as
  `ClaimedElsewhere`, so the sweep reported itself complete, the cursor passed the entry, and the
  delivery was never read again.
