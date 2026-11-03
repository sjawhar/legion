---
title: "Playwright's --project swallows the spec files written after it, so name the files first"
category: testing
tags:
  - playwright
  - dispatch-e2e
  - harness
  - cli
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-527"
  - "sjawhar/legion#2322"
---

# Playwright's --project swallows the spec files written after it, so name the files first

Extends docs/solutions/testing/a-layout-change-is-proven-by-the-whole-project-and-the-rig-needs-contracts-built-and-three-engines-it-cannot-start-INFRA-92.md.

- **Write the spec files before `--project`, or use `--project=<name>`.** `--project` takes a
  list, so the file names after `--project chromium` are read as more project names:
  `bun run e2e -- e2e/a.e2e.ts e2e/b.e2e.ts --project=chromium --project=iphone`.
- **Split a whole-project run to fit a command's time limit.** The suite runs one worker
  (`workers: 1`, `e2e/playwright.config.ts`). In an issue pod the whole `chromium` project did not
  finish in 3,500 s. Run it as two halves of the spec list, and write a run that a limit cut short
  as cut short, naming the files it did not reach.

## Evidence

- INFRA-527: `bun run e2e -- --project chromium e2e/project.e2e.ts e2e/threads.e2e.ts …` failed
  after the one-minute web build with
  `Project(s) "e2e/project.e2e.ts", "e2e/threads.e2e.ts", … not found. Available projects:
  "chromium", "iphone", …`. With the files first and `--project=chromium`, the same run gave
  25 passed, 1 skipped.
- The whole `chromium` project at 89bf76b8 reached test 781 (755 passed, 25 skipped, 1 failed)
  before `timeout 3500` stopped it. `typed-blocks`, `workspace` and `writes` were not reached. The
  `iphone` project ran as two halves of 44 files each: 412 passed, 62 skipped in 31.1 min, then
  246 passed, 70 skipped in 19.5 min.
