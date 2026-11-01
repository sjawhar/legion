---
title: "Read a test runner's counts line, never a tail of its failure list"
category: testing
tags:
  - playwright
  - evidence
  - reporting
  - agent-workflow
date: 2026-10-25
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-95"
  - "sjawhar/legion#2156"
---

# Read a test runner's counts line, never a tail of its failure list

A long run's output is read through a pipe, and the end of it is not the summary.

- **Take the verdict from the counts line**, the one that says how many passed, failed and were
  skipped. Quote that line, not a window onto the output near it.
- **Never conclude from `tail`.** Playwright's list reporter prints `N failed` and then the names
  of the failing rows, each **without a status mark**; a tail that starts inside that list shows
  lines that look exactly like the lines of a pass list, with the `N failed` above the window.
- **When a run is too long for one window, filter for the counts** (`grep -E "passed|failed"`) or
  read the reporter's own machine-readable output. A `tail` is for watching, never for reporting.
- **A number you report is a claim.** If a second reader's totals disagree with yours, re-derive
  yours from the counts line before defending it; the cheapest check is usually one command that
  answers the question directly rather than another full run.

## Evidence

INFRA-95's implementer ran five Playwright projects in one leg and reported "51 passed, 2 skipped,
0 failed" for the group. The run had in fact failed 57 rows — every `webkit` and `webkit-iphone`
row, at `browserType.launch` — and the 51 passes were the three remaining projects. The output had
been read with `tail -25`, which began inside the failing-row list and cut the `57 failed` line
above it. The error reached a phase handoff and a pull request body, and the tester's own run of
the same group is what found it; a single standalone command
(`bunx playwright screenshot --browser webkit about:blank <file>`, which refuses and writes no
file) then settled it in seconds without repeating the leg.
