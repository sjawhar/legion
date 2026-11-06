---
title: "Price a fail-wide rule on replayed heads, and pin the steps it widens instead of widening their filters"
category: github
tags:
  - ci
  - paths-filter
  - runner-minutes
  - measurement
date: 2026-10-29
status: active
module: .github/scripts
related_issues:
  - "sjawhar/legion#2316"
---

# Price a fail-wide rule on replayed heads, and pin the steps it widens instead of widening their filters

- **Measure the minutes before you accept a rule that widens a gate.** Take every pull-request
  head of a recent window (several days, all non-draft heads). Run each head's own diff (the
  compare API, `main...<head>`) through the filters before and after the change. Cost each newly
  selected job at its minutes in that head's own run, else at the job's median over the window.
  Run the replay in reverse too: a rule that "only widens" must deselect nothing.
- **Prove the instrument detects widening.** Run the same script against a revision you know
  widens, and report that number beside the result.
- **Where the price is large, pin instead of widening.** A step whose commands the check cannot
  reduce (a loop, a pipe, `GOOS=… go build`, `-ldflags`) can be pinned by its whole script text and
  working directory, with the closures it stands for listed beside it (one per `GOOS`/`GOARCH` it
  loops over). Its filter then stays the closure. Any edit to the pinned text falls back to
  reading everything, so the pin cannot hide drift; it only defers the cost until the step
  changes.
- **Put the number on the pull request** (its minutes section), so the person who decides on the
  rule sees what it costs.

## Evidence

sjawhar/legion#2316: 187 pull requests, 1,201 heads over 3 days, 1,149 of them non-draft.

- Round 9's fallback (df1f30f5) made `envoy_go` and `envoy_smoke` select any Go change. That
  newly selected 161 and 149 heads, +4,382 minutes, about 1,460 a day against a projected saving
  of about 2,600 a day. The architect reversed the decision on that number.
- 75722984 pinned envoy-go's unit-test and pack steps and envoy-smoke's plugin-schema step
  (`pinned_step`, `.github/scripts/ci_job_reads.py:64`). Both filters then selected exactly the
  heads they had before. Only `go_notices`, whose continued subshells stay unpinned, newly
  selected 27 heads (+163 minutes), and the reverse replay deselected none.
- The replay scripts were throwaway (`collect.py` and `analyze.py` under the issue's `$TMPDIR`).
  Rebuild them from the first rule above.
