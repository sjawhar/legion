---
title: "A memo of a locked decision keys on every input, and never on a value read without the lock"
category: daemon
tags:
  - caching
  - locking
  - races
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/docs
---

# A memo of a locked decision keys on every input, and never on a value read without the lock

Extends docs/solutions/daemon/a-kept-field-changes-every-gate-keyed-on-it-test-the-reopen-a-linger-change-exists-for.md.

- A memo's key must hold every input the remembered decision reads. When a change adds an input to
  a decision, find every memo of that decision and add the input to its key.
- When the decision is taken under a lock, an input read outside that lock must not key the memo.
  Either re-read it under the lock and compare, or write the memo only when the unlocked read agrees
  with the locked decision.
- Prove the lock half with a test hook between the unlocked read and the lock, which changes the
  input in that window.

## Evidence

The block index's live-ahead cache in sjawhar/legion#2233 (`packages/envoy/internal/dispatch/docs/block_index.go`):

- fd3bfa69, a missing input. Main's cache was keyed on version, `marked_at` and no resident room.
  This change made "issue open" an input to the skip, and a close can leave the version and the mark
  unchanged, so a closed document stayed cached at `LiveAhead:1 OwedDocuments:1`
  (`TestBlockIndexRereadsALiveAheadDocumentWhoseIssueCloses`).
- e99160d9, an input read without the lock. The listing reads `i.closed_at is null` with no lock, and
  `lockArtifactOwner` decides under it. A reopen between the two cached the listing's closed value,
  and after a second close the document was never indexed. The cache is now written and reused only
  for a listing that reads the issue open. `TestBlockIndexRemembersNoLiveAheadDocumentItListedClosed`
  replays the interleaving through the `afterBlockIndexListed` hook.
- The same function already used the right pattern for its live text: it indexes text read before the
  lock only while `marked_at` under the lock equals the listed value.
