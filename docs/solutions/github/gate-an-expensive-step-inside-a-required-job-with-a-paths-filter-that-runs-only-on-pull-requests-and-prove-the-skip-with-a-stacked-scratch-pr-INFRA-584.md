---
title: "Gate an expensive step inside a required job with a paths filter that runs only on pull requests, and prove the skip with a stacked scratch PR"
category: github
tags:
  - ci
  - paths-filter
  - required-checks
  - scratch-pr
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-584"
  - "sjawhar/legion#2328"
---

# Gate an expensive step inside a required job with a paths filter that runs only on pull requests, and prove the skip with a stacked scratch PR

A required job (here `lint` in `pr-and-main.yaml`) must keep reporting on every pull request, so do not gate the job. Gate the costly steps inside it:

1. Add a `dorny/paths-filter` step with `if: github.event_name == 'pull_request'`. Its filter lists every file the gated steps read: the source trees, the scripts they run, the config files they read, and the workflow file that holds them.
2. Give each gated step, including its cache restore and save, `if: github.event_name != 'pull_request' || steps.<filter id>.outputs.<filter> == 'true'`. On a push to main and on a merge-queue run the filter does not run and its output is empty, so the step always runs there.
3. A new `uses:` trips the repository's guard scripts, which refuse any name they do not list. `check-image-trigger-paths.sh` matches `NON_BUILDING_ACTIONS` by exact name, so `actions/cache` does not cover `actions/cache/restore` or `actions/cache/save`. `check-bun-version.sh` rule 8 lists every input the setup-bun wrapper may pass. Add the new name with a test case in the same change, and run both `.test.sh` files before you push.
4. Because the filter lists its own workflow file, the pull request that adds it runs the gated steps. Prove the skip with two scratch pull requests whose base is the issue branch (`legion/<KEY>`), so each diff is one file: one changes a file outside the filter and must show the steps skipped, and one changes a file inside it and must show them run. Make each scratch edit pass the repository's own lint, for example `gofmt` for a Go comment. Close each scratch pull request and delete its branch once its run has settled.

## Evidence

- sjawhar/legion#2328, `pr-and-main.yaml` at 8e0352b9: the `notices_inputs` filter and its gated restore, notices step and save.
- Scratch #2329 changed one TypeScript file, run 40447991840: the filter ran, the restore, the step and the save were skipped, and lint took 54 s against an 11.82 min median before.
- Scratch #2330 changed a Go file, run 40453966804: the step ran from 06:16 to 06:31Z. Its first run, 40447995557, failed at `gofmt` on the scratch comment, so the gate was never reached.
- The two allowlist additions with their test cases are in 47d442dd. Review threads PRRT_kwDORFy7ds6wk0MZ and PRRT_kwDORFy7ds6wpqzY corrected rule 8 of `check-bun-version.sh`, in d317a9a5 and 8e0352b9.
