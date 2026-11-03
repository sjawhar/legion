---
title: "Moving a browser row between spec files changes the engines that run it"
category: testing
tags:
  - playwright
  - dispatch-e2e
  - coverage
date: 2026-10-29
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2502"
---

# Moving a browser row between spec files changes the engines that run it

Extends docs/solutions/testing/a-browser-test-of-an-editor-node-view-scrolls-the-drop-target-first-clicks-far-below-the-fold-with-nothing-focused-in-firefox-and-proves-in-app-navigation-with-a-page-marker-LEGION-695.md.
That document covers adding a **new** spec to the `firefox` project's `testMatch`. The delta is
the move: in `packages/dispatch/e2e/playwright.config.ts` the file a row lives in, not the row,
decides which engines run it, so a move that touches no assertion can still drop two engines.

- **`chromium` and `iphone` take every spec by exclusion; `webkit` and `firefox` take a list of
  file names.** Read both `testMatch` regexes before you move a row, and pick a destination that
  is in them when the row must keep four engines.
- **Confirm with the runner, not with the regex you just read:**
  `bunx playwright test --config e2e/playwright.config.ts <file> --project <p> --list` prints
  `Total: 0 tests in 0 files` for a file that project does not select, and naming such a file in a
  run makes Playwright exit non-zero rather than quietly running nothing.
- **A helper module must not be named `*.e2e.ts`.** The root `testMatch` is `/.*\.e2e\.ts/`, so a
  file named that way is collected as a spec wherever it sits. Name it without the suffix, and
  move into it the `declare global` `Window` keys its init scripts write, so one file owns each
  key.
- **When the move does cost engines, say so in the plan as a departure and let the architect
  choose**, between the destination that keeps them and adding the file to both lists at the cost
  of running that file's other rows in two more engines.

## Evidence

sjawhar/legion#2502 moved a past-version landing row out of `e2e/mermaid.e2e.ts`. The spec named
`e2e/doc.e2e.ts` as its destination; `doc` is in neither the `webkit` nor the `firefox`
`testMatch`, while `mermaid` and `deep-links` are in both, so that move would have taken the row
from four engines to two. The plan recorded it as a departure with the two alternatives, and the
architect chose `e2e/deep-links.e2e.ts`, which keeps all four and is itself a deep-link row.

The probe is the tester's: `e2e/doc.e2e.ts --project webkit --list` reported
`Total: 0 tests in 0 files`. At the head the row ran in chromium, iphone, webkit and firefox.

The same change added `packages/dispatch/e2e/mermaid-helpers.ts`, deliberately without the
`.e2e.ts` suffix, and moved four `Window` keys into its own `declare global` so no key is declared
in two files.
