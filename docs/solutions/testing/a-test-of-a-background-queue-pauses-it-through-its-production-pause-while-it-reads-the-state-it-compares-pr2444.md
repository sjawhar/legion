---
title: "A test of a background queue pauses it through its production pause while it reads the state it compares"
category: testing
tags:
  - race
  - flaky-test
  - background-queue
  - go-test
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A test of a background queue pauses it through its production pause while it reads the state it compares

- When writes can wake a background queue and a test reads "before" state that the queue
  changes, hold the queue across those reads. Use the pause that production already has
  (Quiesce's), not a test-only knob or a sleep.
- The pause drops the notes taken while it holds. After `resume()`, note the item yourself, as
  the writes did. Prove the writes note it in an earlier, unpaused part of the test.
- A test that sets a zero delay can start to race when a production change makes the queue act
  on every note. Re-run every test of that queue under `-race -count=50` after such a change.
- Prove the fix with the same command on the old test file. It must fail as CI failed.

## Evidence

- `packages/envoy/internal/dispatch/docs/compaction_queue.go`: `pause` returns `resume`.
  `Quiesce` uses it (`defer s.compactions.pause(ctx)()` in `docs/service.go`).
- Round 2's fix made a note bring a waiting room's check forward, which the tests' `firstCheck`
  of 0 turned into a check at once. In envoy-go-race at `3e89ac6f` (run 40876511289),
  `TestTheCompactionQueueFoldsTheOldRowsOfADocumentItsWritesNoted/browser_edits` timed out:
  the queue compacted before the test took its step count.
- `35636880` pauses the queue in that test. Under `-race -count=50`, 150 of 150 subtests pass,
  against 144 of 150 for the file at `3e89ac6f`.
