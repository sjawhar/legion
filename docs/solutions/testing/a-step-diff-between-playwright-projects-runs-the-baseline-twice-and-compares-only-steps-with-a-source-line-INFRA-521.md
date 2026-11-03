---
title: "A step diff between Playwright projects runs the baseline twice and compares only steps with a source line"
category: testing
tags:
  - playwright
  - reporter
  - test-selection
  - dispatch
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-521"
  - "sjawhar/legion#2313"
---

# A step diff between Playwright projects runs the baseline twice and compares only steps with a source line

To find the tests whose path differs between two Playwright projects (for example `chromium` and a
phone project), as a cross-check on a reading of the code:

- Record each test's steps with a throwaway reporter passed on the command line
  (`--reporter=list,$TMPDIR/steps-reporter.ts`), so the config stays unedited. Key each test by
  file › title path, never by line.
- Keep only steps of category `pw:api`, `expect` and `test.step`, and identify each step by its
  normalized title plus `step.location` (file and line). Titles alone are generic
  (`Expect "toHaveCount"`); the location shows which branch ran, helpers included.
- Drop steps with no location, such as `Launch browser`, `Create context` and `Close context`. A
  worker restart after any failure adds them, so they flag tests whose code took no other path.
- Run the baseline project twice and count a test as different only when the other project takes
  a step that neither baseline run took. Treat a missing step as no evidence.
- A test that failed or timed out has an incomplete step list. Re-run it alone before you read it.
  The diff cannot see values or `page.evaluate` bodies, so it only adds to a reading; it never
  clears a test on its own.

## Evidence

sjawhar/legion#2313: at base 1357b95c the whole `chromium` project ran twice and `iphone` once in an
issue pod. Before location-less steps were dropped, the diff flagged two tests the reading had
correctly dropped (the ask-blocks copy row, the board filter strip) only on `Launch browser`. With
located steps only, 119 passing tests had an `iphone`-only step, all of them keep rows, and the
diff and the reading disagreed on 0 of 786 tests. `ask-blocks.e2e.ts:459` timed out under load and
was re-run alone with a longer timeout before it was read.
