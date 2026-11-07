---
title: "A measured CI window is read after it closes, and every statistic names its population"
category: best-practices
tags:
  - measurement
  - ci
  - github-api
  - evidence
date: 2026-11-03
status: active
module: .github
related_issues:
  - "sjawhar/legion#2726"
---

# A measured CI window is read after it closes, and every statistic names its population

Extends docs/solutions/best-practices/an-instrument-that-answers-a-smaller-question-returns-the-comfortable-answer-INFRA-141.md.

That note's shape is the instrument that answers a narrower question than the one asked, and
returns the answer that lets the work go on.
Three instruments it does not list, all of which shipped wrong answers through an implementer, a
tester and a reviewer in one pull request:

- **A window read before it closed.**
  When the criterion is "the newest N jobs created before T", and a job counts only once some
  step of it has reached a terminal state, read the window only after every job created before T
  has ended.
  A job still running is already a member of the window that has not yet qualified, so reading
  early does not leave a gap you can see: it silently pulls N older jobs in and reports a window
  that is not the one asked for.
- **An aggregate over a population the rule did not name.**
  "The largest green duration of a step" is not the largest duration of that step.
  Filter on **that step's own conclusion** before you take the maximum, not the job's conclusion
  and not nothing.
  The rule matters most exactly when it is easiest to skip: in a window measured to size a
  timeout, the longest run of the step **is** the timeout you are fixing, so the unfiltered
  maximum is the defect's own number.
- **Record why each candidate is out, read rather than inferred.**
  A selection rule is a filter, so it is checked by what it refuses: for every job of the window
  you do not count, record the fact that excluded it as GitHub gives it — the job's conclusion,
  the deciding step's conclusion, or the annotation's text.
- **A figure you did not measure carries its source's population, not your sentence's.**
  Copying a number out of a comment, a pull request body or another document copies a claim about
  a set you have not looked at.
  Copy the population with the number, or re-measure before you write it: correcting one figure of
  a sentence re-opens every other figure in it, and the one you filled in by copying is the one
  that costs the next round.

## Evidence

sjawhar/legion#2726 (dispatch://INFRA-1052) sized a CI job's `timeout-minutes` from the 30 newest
counted `test` jobs of main push runs.
That population has since stopped existing: `ci.yaml` runs on a pull request, a merge group and a
dispatch, and on no push, so the same window read today is the merge-group runs' jobs, which is
where a change is tested on the tree it lands as.

- **The window.** The implementer read it at about 15:55Z while three jobs of it were still
  running, so none of them counted yet: jobs 126005315512, 126005997200 and 126006026475, all
  created by 15:13Z, before the pull request opened at 16:56:21Z, and all ended by 17:06Z.
  The table therefore carried three jobs that were too old, and the tester failed the round on it.
  Re-read with the pull request's own open time as the cutoff, after those jobs had ended, it is
  the right 30.
- **The aggregate.** The limit's comment counts each `go test` step at "its largest measured green
  duration plus its own `-timeout`".
  The implementer took the race step's maximum over all 30 jobs, 734 s — the step of job
  125691048644, which concluded `failure`: the job that ran out `go test`'s old ten-minute
  default, which is the failure the whole issue was about.
  28 of the 30 race steps are green and the longest of them is 694 s (job 125690118806).
  The reviewer caught it at the approved head, and the sum fell from 4618 s to 4578 s.
- **The same error twice, from two readers.**
  The tester had computed the green subset for the plain step and taken the race step's maximum
  unfiltered, so it verified the implementer's number against a figure made the same way, and
  recorded 734 s in two of its own handoffs before correcting them.
  Two independent checks agree when both apply the same wrong filter, which is why the rule has to
  be stated as the population and not as a habit.
- **The refusals.** 13 runs of the window carry no counted job, each for a reason read from
  GitHub: four with no `test` job, one skipped job, one whose race step is `skipped` under the
  annotation `repository transferred`, and seven cancelled by a bot.
  The near case is job 125771373229, which reached its race step and ran 3 m 41 s of its 30
  minutes before the cancel, and is still out.
- **The inherited figure.** The same pull request's `docs/testing.md` paragraph said
  `internal/daemon` "took up to 694 s without `-race`", which no population gives: 694 s is the
  race step's maximum in the spec's table.
  The implementer measured the replacement for that half of the sentence and filled the other
  half — "306 to 570 s under `-race`" — by copying `.github/workflows/ci.yaml`'s race-step
  comment, which names "the green runs of 2026-11-02 and 2026-11-03".
  The tester then measured that population: over its 39 green main `test` jobs the range is
  322.432 s to 560.390 s, so neither end is attained.
  The figure is really from sjawhar/legion#2709's pull request body, over 51 green `test` jobs of
  those two days, pull-request jobs included.
  One copied number cost a fourth round of a paragraph that had already had three.
