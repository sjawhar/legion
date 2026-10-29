---
title: "A CI negative control for a browser row changes a value the earlier gates accept and runs on its own throwaway pull request"
category: testing
tags:
  - negative-control
  - ci
  - playwright
  - github-actions
date: 2026-10-28
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2470"
  - "sjawhar/legion#2471"
---

# A CI negative control for a browser row changes a value the earlier gates accept and runs on its own throwaway pull request

- **Break the behaviour so that the lint, typecheck and unit steps still pass.** In the CI job,
  those steps run before the browser step. A deleted code path makes a unit test or Biome fail
  first, so Playwright never runs. Change a value instead. For example, set a limit far past the
  row's expect timeout. Then only the browser row can catch the change.
- **Run the control on a second, non-draft pull request, pushed at the same time as the real
  one.**
  - A workflow whose concurrency group is the pull request number cancels a run that is still in
    progress when the next push arrives. So a red commit on the issue PR would lose its run, or
    would hold back the real push.
  - Draft pull requests run nothing.
  - Use a branch outside `legion/`, which the daemon ignores. Put no Dispatch key in the branch,
    the title or the commits. Say "not for merge" in the title and the body.
- **Move the control branch forward with a merge commit when the real head changes**
  (`jj new <control> <new head>`). Never force-push it.
- **Quote the row's own `✓` and `✘` lines from the job log** (`gh run view <run> --job <id> --log`).
  Do not use the job's overall result: another row can be red in the same job.
- **Close the control pull request with `--delete-branch`** once its lines are quoted on the real
  pull request.

## Evidence

sjawhar/legion#2470: `MarkdownBody.test.tsx:1341-1353` unit-tests the Mermaid refusal, so a removed timer
would fail the `dispatch` job's unit step before Playwright ran. The control #2471 set
`RENDER_TIME_LIMIT_MS = 600_000`, and the unit tests stayed green (295 pass in `web/src/features/refs`).

| Run | Pull request | The row | Totals |
|---|---|---|---|
| 40732919704 | control #2471 | `✘` in chromium, iphone, webkit and firefox, all at the refusal `toHaveText` (`mermaid.e2e.ts:269`) | 4 failed, 1207 passed |
| 40732897855 | #2470 | `✓` in the same four projects | 1211 passed |

During planning, `recipients-claim.e2e.ts:310` was red in the same job on other pull requests. The
`dispatch` job's result could therefore not be the evidence.
