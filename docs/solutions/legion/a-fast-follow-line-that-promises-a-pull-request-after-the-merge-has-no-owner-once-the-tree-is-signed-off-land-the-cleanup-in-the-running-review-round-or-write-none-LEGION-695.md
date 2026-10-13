---
title: "A Fast-follow line that promises a pull request after the merge has no owner once the tree is signed off: land the cleanup in the running review round, or write none and let the architect file it"
category: legion
tags:
  - legion-worker
  - legion-architect
  - fast-follow
  - review-round
  - simplify-pass
date: 2026-10-12
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-695"
  - "sjawhar/legion#1928"
---

# A Fast-follow line that promises a pull request after the merge has no owner once the tree is signed off: land the cleanup in the running review round, or write none and let the architect file it

Extends `docs/solutions/legion/fast-follow-pr-mechanics-and-queue-registration.md`, which says how
a `Fast-follow:` line becomes one same-issue pull request after the parent merges. The delta is
when that shape is not what the architect wants:

- A line reading "a follow-up pull request on this issue, after this one merges" is a deferral
  with no owner: the architect signs the tree off on the production check, and nothing then
  starts a worker on the line. The architect of LEGION-695 ruled that the line must name where the
  cleanup really lands, in two cases: the cleanup is done in the review round already running (a
  round that goes through the tester and the reviewer anyway costs no extra cycle for a small,
  behaviour-preserving change), or the line reads `none` and the architect files the cleanup as an
  issue of its own.
- So when the simplify pass or a reviewer names a duplication or a file-placement cleanup, judge
  it then: small and behaviour-preserving with tests that already cover both callers — do it in
  the round that is open and write `none — landed in <commit>`; anything larger or with behaviour
  in it — write `none` and tell the architect what to file. Never write the promise.
- A cleanup landed this way is a code commit above the round's fixes: it re-runs the suites the
  round runs anyway, needs no new real-surface proof when it changes no behaviour, and the
  reviewer's next pair reads it with the rest.

## Evidence (LEGION-695, sjawhar/legion#1928)

The round-1 simplify pass found that `pictureSerializer` and the new `pictureView` each derived a
picture's `dispatch://` source test and alt-or-slug caption, and the PR body's `Fast-follow:` line
promised a follow-up pull request. The architect's round-2 message (Envoy, 2026-10-12 16:45Z)
asked for the helper in that round or `none`; `pictureReference` landed in cee69bb1 with seven unit
cases, outputs unchanged. Round 4's reviewer named a second cleanup (the Firefox caret wiring split
across `ask-block.ts` and `picture-view.ts`) as a Fast-follow; the architect repeated the rule and
`pictureNodeView(view)` moved into `picture-view.ts` in af766f05, the same push as the
documentation fixes. The line ended `none — … both at the architect's request; nothing is deferred`.
