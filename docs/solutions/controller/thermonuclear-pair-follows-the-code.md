---
title: "A fix commit with production logic gets its own scoped thermonuclear pair"
category: controller
tags:
  - controller
  - merge-queue
  - thermonuclear-review
  - code-review
  - pr-gate
date: 2026-09-09
status: active
module: daemon
problem_type: convention
component: development_workflow
severity: high
applies_when:
  - A PR gate's thermonuclear pair (deep + quality review) already ran once on the full diff
  - A subsequent fix commit in the same PR changes production logic, not just docs or test-only changes
  - The merge-queue controller is deciding whether that fix commit needs review before merge
---

# A Fix Commit With Production Logic Gets Its Own Scoped Thermonuclear Pair

## Context

`sjawhar/legion#826`'s PR gate ran a full thermonuclear pair (deep + quality review agents) on
the complete diff, which found eleven correctness defects. A fix commit (20 files) addressed
all eleven with regression tests. The merge-queue controller then had to decide: does a fix
commit that only repairs findings the pair already surfaced need its own review, or is it
covered by the original pair's verdict?

The controller ruled that it does — "the pair follows the code" — and dispatched a second,
scoped thermonuclear pair against that fix commit specifically. That scoped pair found three
more real correctness defects that the fix commit itself introduced or left in place: an
inverted lock order between the edit path and the ask/comment answer paths (a genuine
Postgres deadlock under concurrency, not present in the original eleven), an out-of-bounds
anchor range returning 400 before its quote-fallback path could run, and a nearest-occurrence
guess on repeated text that could silently attach an anchor to the wrong occurrence. None of
these three were among the original eleven findings — they were new defects in the fix itself.

## Guidance

- **A fix commit that touches production logic is not "the same diff, already reviewed" — it
  is new code and gets its own scoped review**, even when its stated purpose is only to resolve
  findings from a review that already happened. The original pair reviewed the code as it
  stood before the fix; it has no opinion on what the fix commit actually wrote.
- Scope the second pair to the fix commit's diff, not the whole PR again — this keeps the cost
  proportional (20 files, not 288) while still catching defects the fix introduces.
- This is not a permanent one-more-round tax: a docs-only or test-only follow-up commit needs
  no such pass. The trigger is production logic in the commit, not merely "a new commit
  exists".

## Why This Matters

Review findings are fixed under time pressure, often by an agent working from a list of
findings rather than re-deriving the design from scratch — exactly the condition under which a
fix introduces a new, unreviewed defect (here, a lock-order inversion that would deadlock two
otherwise-correct-looking code paths under concurrency, the kind of defect that single-path
testing does not surface). Treating "the pair already ran on this PR" as blanket coverage would
have let three real defects, including a production deadlock, merge silently. The scoped
second pass turned up a 3-in-20-files hit rate on code whose only purpose was to fix findings —
evidence that fix commits are not lower-risk than the code they fix.

## When to Apply

Any PR gate in this repo's merge-queue process where a thermonuclear pair already ran and a
subsequent commit changes production logic (Go, TypeScript application code, SQL migrations,
CI logic with behavioral effect) — not limited to Dispatch; this is a merge-queue-controller
convention, not a Dispatch-specific one.

## Examples

The ruling as recorded live in the SDD ledger:

```
Controller ruling 19:01Z: pair follows the code -> scoped pair on the fix commit (20 files).
deep: 2 CORRECTNESS (issue-vs-row lock order inverted between edit path and ask/comment
actions -> Postgres deadlock under concurrency; out-of-bounds range with quote returns 400
before quote fallback). quality: 1 CORRECTNESS (nearest-occurrence guess on repeated text can
attach to the wrong occurrence) + 3 CLEANUP.
```

Confirmed independently in the PR's own merge record: "Merge-queue controller: gates held at
the final fix commit (12/12 green, 0 threads, scoped pair on the fix commit: 3 correctness
fixed in-PR with failing-first tests; live re-probe of the anchor paths). Squash-merging."

## Related

- `.superpowers/sdd/2026-09-09-dispatch-native-workspace/progress.md` — the ledger entries for
  the fix commit and its own follow-up fix, both part of `sjawhar/legion#826`.
