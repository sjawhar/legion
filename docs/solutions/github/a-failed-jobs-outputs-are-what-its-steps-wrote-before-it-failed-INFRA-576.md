---
title: "A failed job's outputs are what its steps wrote before it failed, so a job that acts on part of a failed job tests the result and an output only the certifying step writes"
category: github
tags:
  - github-actions
  - job-outputs
  - job-conditions
  - always
  - needs-result
  - release-workflow
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "INFRA-576"
  - "sjawhar/legion#2358"
---

# A failed job's outputs are what its steps wrote before it failed, so a job that acts on part of a failed job tests the result and an output only the certifying step writes

Extends docs/solutions/github/a-job-condition-downstream-of-a-skippable-job-calls-always-and-tests-the-result-of-every-job-whose-outputs-it-reads-LEGION-779.md.

## The rule

1. Downstream of a job that failed, `needs.<job>.outputs.<name>` is whatever its steps wrote to
   `$GITHUB_OUTPUT` before the failure. It reads as `''` only when the job was skipped or no step
   wrote it. LEGION-779's "a skipped or failed X's outputs read as ''" holds only for outputs the
   failed job never wrote.
2. When one job does several independent things (several packages in one publish job) and a later
   job must act on the parts that succeeded, test `needs.<job>.result == 'success' ||
   needs.<job>.result == 'failure'` together with an output that only the step certifying that
   part writes, on that step's last line, and make the empty value the safe one.
3. Inside the job, chain each part's steps on `if: ${{ !cancelled() && steps.<previous>.outcome
   == 'success' }}`, so one part's failure stops its own steps and no other part's.

## Evidence

- `.github/workflows/release.yaml` line 899: `publish`'s legion release step writes `version` and
  `tag` only after `release-push.sh release` succeeds. Lines 1053-1057: `worker_image_release`
  tests `needs.publish.result == 'success' || needs.publish.result == 'failure'` and
  `needs.publish.outputs.tag != ''`. Line 1039: an empty `pair_ok` sets `skip_attest`, so nothing
  is attested.
- Probe run 40451765270 (sjawhar/legion-smoke, the branch's exact job graph with stub steps): a
  forced failure of `publish`'s last step, after legion released, still let
  `worker_image_attest / attest` and `worker_image_release` read `pair_ok` and `tag` from the
  failed `publish` and run. Probe run 40451765293: a forced failure of pi-envoy's npm step left
  `pair_ok` false and the legion steps' `tag` unwritten, so both stayed off.
- The same runs show `!cancelled()` running `worker-image.yaml`'s attest job past a build job its
  own `if:` skipped (`worker-image.yaml:304`), the case LEGION-779 lists as not proven.
