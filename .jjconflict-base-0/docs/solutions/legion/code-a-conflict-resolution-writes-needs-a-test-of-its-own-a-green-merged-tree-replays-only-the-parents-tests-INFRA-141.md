---
title: "Code a conflict resolution writes needs a test of its own: a green merged tree replays only the parents' tests"
category: legion
tags:
  - forward-merge
  - conflict-resolution
  - mutation-testing
  - review-round
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-141"
  - "sjawhar/legion#2167"
---

# Code a conflict resolution writes needs a test of its own: a green merged tree replays only the parents' tests

Extends docs/solutions/legion/a-siblings-new-caller-of-a-predicate-you-widened-is-a-compile-error-whose-one-keystroke-fix-restores-the-old-rule-LEGION-668.md.

That note covers a sibling's call site that stops compiling. This one covers any code the merge
writes that neither parent has: a new type, a changed call, a branch that joins the two sides.

- **Before you push a merge that wrote code, write a test that runs that code and fails on its
  mutant.** Each parent's tests reach the paths that parent knew about. The merge's code sits
  where neither parent's tests go, so every suite passes on the merged tree whatever that code
  does.
- **"Existing tests cover it" is a claim: name the test and the mutant it fails on.** A test that
  reaches the right answer through another condition of the same check does not cover the new
  one.

## Evidence

- The forward merge 7ce88c70 joined main's #2302 (the Dispatch outbox's publish window) with this
  pull request in `packages/envoy/internal/bus`. The resolution wrote `flushWindowElapsedError`
  (`confirm.go`): it keeps this branch's spent-window text, which log filters match, and unwraps
  to main's `errFlushWindowElapsed`, which `window.answered` turns into `ErrUnavailable`. It also
  changed `window.republish` (`async.go`) to pass a context.
- Every suite passed on the merged tree, and the handoff said main's window tests covered the
  resolution. Review 5662141896 ran a mutant whose `Error()` returned other text and whose
  `Unwrap()` returned nil: `./internal/bus` and `./internal/dispatch/outbox` still passed. This
  branch's text test goes through `publishConfirmed`'s check before the publish, a different
  exit. Main's window test reaches `ErrUnavailable` through a lost connection, another condition
  of `window.answered`.
- 73008a11 added `TestASpentFlushWindowKeepsItsTextAndIsUnavailableToAWindow`
  (`internal/bus/flush_window_elapsed_test.go`). It drives `confirmFlushed` with a spent deadline,
  then `window.answered`, and fails on the mutant, on a text-only mutant and on an unwrap-only
  mutant.
