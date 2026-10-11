---
title: "A sibling's new caller of a predicate you widened is a compile error whose one-keystroke fix restores the old rule: give it the real argument from the same source, and pin the blank as the negative control"
category: legion
tags:
  - forward-merge
  - sibling-pr
  - predicate
  - function-signature
  - negative-control
  - review-attribution
date: 2026-10-11
status: active
module: packages/daemon/internal/daemon
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
  - "sjawhar/legion#1894"
---

# A sibling's new caller of a predicate you widened is a compile error whose one-keystroke fix restores the old rule: give it the real argument from the same source, and pin the blank as the negative control

- When your branch adds a parameter to a decision function and a sibling lands a new call site of
  it on `main` in a file your branch never touched, the forward merge raises no conflict there: the
  compiler is the only tell (`not enough arguments in call`). Treat it as a behaviour change to
  make, not a signature to satisfy: the zero value that compiles (`""`, `nil`, `false`) is the rule
  your branch replaced, now re-enabled on the sibling's path alone, where the two paths were built
  to agree.
- Feed the new caller from the same source the original caller reads, through one exported helper
  on the engine so both read it one way (here `Engine.ReviewerSession`, the claim's recorded
  session, read once per pull request that has a candidate in a read-only transaction). Then write
  the sibling's own test shape for the case the parameter decides, and run it once with the blank
  in place of the argument: it must fail on the sibling's path the way the webhook path's test
  fails, or the parameter is not reaching the decision.
- Say in the retarget comment that the merge forced a behavioural change and why the compile-only
  fix was wrong: the reviewer re-opens a round on a changed fingerprint and needs to know which
  lines are the merge's own.

## Evidence (LEGION-668, sjawhar/legion#1878; sibling sjawhar/legion#1894)

LEGION-668 gave the workflow's `decidesRound` a `reviewerSession` parameter so a review-App review
decides a round only when its Legion footer names the reviewer's recorded session. #1894 landed on
`main` meanwhile: a periodic read of each open pull request's reviews
(`internal/daemon/review_reads.go`, `reviewToApply`) that calls the newly exported
`Engine.DecidesRound(review)` so a decision whose webhook was lost still ends its round. The
retarget's merge flagged `review.go` (both sides changed the signature line) and nothing in
`review_reads.go`; `go build` failed there. `DecidesRound(resolved, "")` compiles and makes the
read pass apply the controller's newer review every two minutes while the webhook path sets it
aside — a set-aside review records nothing, so `ReviewSeen` never passes it and the pass applies it
again. The merge commit `530e753d` reads the session through `Engine.ReviewerSession` and passes
it; `TestTheReadSetsAsideAReviewAppReviewFromAnotherSession` (the controller's newer
CHANGES_REQUESTED set aside and memoised as refused, the reviewer's older APPROVED moving the issue
to retro, a second pass applying nothing) fails with the blank at its first assertion, `after the
pass the issue is in reviewing, want retro`.
