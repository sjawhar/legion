---
title: "A re-run's green arrives with the required aggregator, and a send-back burns the handoff commit the completion it accepted reported"
category: legion
tags:
  - ci
  - rerun
  - handoff
  - checks-verdict
  - skip-checks
date: 2026-10-31
status: active
module: packages/daemon/internal/classify
related_issues:
  - "sjawhar/legion#2541"
---

# A re-run's green arrives with the required aggregator, and a send-back burns the handoff commit the completion it accepted reported

Re-running a flaked job once is the documented remedy
(`docs/solutions/legion/eleven-passes-on-one-pull-request-a-shared-working-copy-three-heads-and-three-credential-shapes.md`).
What that note does not say is when the daemon can see the result, and what completing too early
costs.

- **Wait for the aggregator, not for the job you re-ran.** `gh run rerun <run> --failed` re-queues
  the failed job *and* every required check that sums it up, and the aggregator queues for a
  runner of its own after the job it sums finishes. Measured here: the `dispatch` job finished at
  20:31:04Z and `ci-ok` at 20:59:52Z, 29 minutes later, with nothing else of the attempt
  outstanding. Read the aggregator's conclusion, and name it when you report the re-run green.
- **A red the daemon still holds is applied by your completion.** `RedSendsBack`
  (`internal/classify/decisions.go`) sends an issue in testing, reviewing, retro or merging back
  to implementing on the verdict `HeadVerdict`
  (`packages/daemon/internal/classify/required.go`) gives the pull request's head. So a completion
  made while the previous attempt's red still stands is not ignored: it is accepted and the round
  is sent straight back. Check the record's `pullRequest.checksVerdict` (the `legion` tool's
  `read_record`) before completing, not only GitHub's job list.
- **That send-back burns the handoff your completion reported.** `engine.go` sets
  `row.LastHandoff = fact.HandoffRecord` on an accepted completion, so completing again naming the
  same record when the round reopens is refused `HANDOFF_NOT_NEW` naming it. Record the handoff
  again — with whatever the round has since learnt — with `dispatch handoff-write`, which writes a
  new version, even when no file of the change moved. The round's second handoff version is the
  expected outcome, not a sign that something went wrong. While a handoff was a committed file
  (before sjawhar/legion#2354) the burnt name was the commit, and the second handoff commit played
  the same part.
- **Nothing validates a handoff's shape, so a field you are no longer meant to write keeps being
  written.** Read your role's own instructions for the fields each round, not the previous
  round's body: this branch's implement handoffs carried a `completed` timestamp, which the
  worker prompt had stopped asking for, and carried no `previousHandoff`, which was then the one
  field that kept two identical rounds from writing the same bytes. Both fields are gone since
  sjawhar/legion#2354 — Dispatch records when each handoff was written and versions it — and the
  rule they illustrate is not: nothing reads a handoff's shape for you.

## Evidence

sjawhar/legion#2541 (INFRA-264), the round after its first retro. CI run 41216990032 attempt 1 at
`61e45835` was red on `ci-ok` alone, because the `dispatch` job failed one Playwright row. Attempt
2 of that run was green (`dispatch` 20:31:04Z, `ci-ok` 20:59:52Z). The implementer completed at
`1fd41445` while the daemon still carried attempt 1's red; the issue came straight back to
`implementing` with `checksVerdict: red`, and the next completion at the same commit was refused
`HANDOFF_NOT_NEW` naming `1fd41445`. The round ended with a second handoff commit, `a955530d`,
holding the same evidence plus the settled CI result — which is why the span between
`61e45835` and `a955530d` carries two implement handoffs and no change to the product at all.
