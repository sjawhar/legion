---
title: "A change that breaks a checker's precondition makes the checker count what it can no longer compare"
category: testing
tags:
  - checkers
  - compaction
  - stored-order
  - false-pass
date: 2026-11-02
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A change that breaks a checker's precondition makes the checker count what it can no longer compare

- When a change makes something happen that a checker assumed would not, find every check that
  relies on that assumption in the same change. A checker that silently skips the cases it can no
  longer compare reports clean over work it never did.
- Make the checker count those cases under a name of their own, print why, and report a subject
  whose every case falls there as unchecked, never clean. Keep the existing rule that a run which
  compared nothing exits non-zero.
- Correct the instructions that stated the assumption. "Run it while nothing is compacted" is an
  instruction no setting can meet once compaction runs on its own.
- Pin it with a test that has cases on both sides of the line: one subject clean only on what it
  compared, and one with nothing comparable, which must come out unchecked.

## Evidence

- `envoy-dispatch stored-order` (from sjawhar/legion#2496) skipped a version whose
  `doc_update_version` is below the oldest stored row, because before #2444 only a room's close or
  the daily pass compacted. #2444's queue compacts a resident room within minutes, so a check
  window longer than about two minutes was compared only from its last fold, and the command
  still exited 0. Review round 15 found this.
- b3a91c12 counts such versions as `versions_compacted`, with a reason naming the base. A
  document left with no version compared is unchecked, and exits 1 when `--artifact` named it.
  The README no longer says "nothing is compacted". The new subtests of `TestStoredOrderChecker`
  and the second document of `TestStoredOrderSubcommandExitCodes` fail with the old bucketing.
- On the real binary, the compacted e2e document reports `versions_checked=1
  versions_compacted=31`. With that one version deleted, it reports `unchecked=1` and exits 1.
