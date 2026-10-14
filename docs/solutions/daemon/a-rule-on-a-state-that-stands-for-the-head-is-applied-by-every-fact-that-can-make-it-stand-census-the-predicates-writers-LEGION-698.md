---
title: "A rule on a state that stands for the head is applied by every fact that can make it stand: census the predicate's writers before calling the rule done"
category: daemon
tags:
  - workflow-engine
  - standingRed
  - checks-red
  - webhook-ordering
  - predicate-writers
  - review-round
date: 2026-10-14
status: active
module: packages/daemon/internal/workflow
related_issues:
  - "LEGION-698"
  - "sjawhar/legion#1950"
---

# A rule on a state that stands for the head is applied by every fact that can make it stand: census the predicate's writers before calling the rule done

- A workflow rule of the shape "when X stands for the pull request's head, move the issue"
  (`workflow/review.go` `standingRed`: a red `classify.RedSendsBack` sends the work back to
  implementing) is not a property of the fact that usually makes X true. List every input the
  predicate reads (`CheckedHead`, `Failing`, `Required`, `Workflows`, `Pushes`, `HeadSHA`,
  `PlannedRed`, the fix-attempt fields) and every handler that writes one of them (the CI
  settlement, the required-set read, the push classification, the synchronize, the hold's retry,
  a reopen), and give each handler the rule, in whatever phase the issue is in when it runs. A
  handler that can make X true and does not apply the rule leaves the issue standing on a state
  the rule says moves it, with nobody told.
- Write the census as a table in the predicate's doc comment or the test file (input → writer →
  handler), and one test row per writer, before the first round completes. Then state the
  invariant once in the rule's doc ("applied by whichever fact makes a red stand, never by a
  review or a completion") rather than listing the facts: the next writer anyone adds has a
  sentence to be measured against, and a list invites the belief that it is complete.
- The carry makes two webhooks writers of one state: GitHub delivers a branch's `push` and the
  pull request's `synchronize` in either order, so a code head's settlement comes to stand for the
  handoff head above it either when the push is classified after the head arrived
  (`classify.SettlementFor` through the push handler) or when the head arrives over a push already
  known (`classify.AdvancePullRequestHead`, `keptPushes` through `pullRequestSynchronized`). Any
  rule over the carried state needs both handlers, and a phase gate on one of them (the push
  handler's `issue.Phase != phase.Reviewing`, which also gated the round's re-ask) hides the gap in
  the phase the acceptance test happens to exercise.
- A hold (`claimFailed` → `phase.Held`) is a writer too: a fact that lands while held moves
  nothing (`Table` has no held row), so the retry that restores the phase is the fact that lets
  the state stand there, from whatever phase the hold was taken, not only the one the existing
  test covered.
- The rule has one definition and the handlers call it; the phase exclusions live inside it
  (`standingRed` refuses `awaiting_merge`, where only the head's own red withdraws a READY), so a
  new caller cannot forget one.

## Evidence

sjawhar/legion#1950 (LEGION-698) moved the CI rule out of the review round into `standingRed` and
took three rounds to reach every writer. Round 1 applied it in `decideChecks` (settlement and
required-set read) and in the push handler for reviewing only; the tester's red test
(`TestAPushClassifiedLateThatCompletesTheCarryOfARedSendsTheWorkBackFromEveryPhase`) showed the
late push left testing, retro and merging standing on a red. Round 2 applied it in the push
handler for every phase; the reviewer's census then found the synchronize over a known push
(`pullrequest.go` `pullRequestSynchronized`) and the hold retry from a phase other than reviewing
(`engine.go` `retryOrEscalate`) applying nothing. Round 3 added both
(`TestAHeadArrivingOverAKnownPushThatCompletesTheCarryOfARedSendsTheWorkBackFromEveryPhase`,
`TestARetryOfAHoldFromAnyPhaseWithARedHeadSendsTheWorkBack`) and wrote the invariant into
`standingRed`'s doc. An oracle pass in round 1 had tabulated the predicate's inputs against their
writers and found the synchronize uncovered; the table was read as "cannot newly make a red
actionable" for the head move alone, and the carry case was missed. The same shape in another
domain is
`docs/solutions/daemon/dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md`
(enumerate the existing writers before adding one).
