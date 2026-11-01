---
title: "A job condition downstream of a skippable job calls always() and tests the result of every job whose outputs it reads, because the implicit success() is transitive through a job that ran"
category: github
tags:
  - github-actions
  - job-conditions
  - implicit-success
  - always
  - needs-result
  - release-workflow
  - check-release-credentials
date: 2026-10-16
status: active
module: .github/workflows
related_issues:
  - "LEGION-779"
  - "sjawhar/legion#1975"
  - "LEGION-748"
---

# A job condition downstream of a skippable job calls always() and tests the result of every job whose outputs it reads, because the implicit success() is transitive through a job that ran

## The rule

1. **Every job `if:` downstream of a job that can skip takes one shape:** `always() &&
   needs.<X>.result == 'success' && …` for every `X` whose `needs.<X>.outputs` it reads, before
   the read. GitHub gives an `if:` that names no status function an implicit `success()`, which is
   false when **any** job upstream through `needs`, at any distance, was skipped (actions/runner
   issue 491). The condition's own text then never runs.
2. **The skip is not consumed by a job that ran.** An `always()` job between the skipped job and
   yours, green and with its outputs set, changes nothing: your implicit `success()` still sees
   the skip. Review the whole ancestor chain, never only the direct predecessor.
3. **Two shapes that look careful are the same skip.** An explicit `success() && …` is the
   implicit one. `needs.X.result == 'success' && needs.X.outputs.skip != 'true'` with no status
   function is skipped before the result test is read.
4. **`always()` alone is the opposite hazard.** Under `always()` a skipped or failed `X`'s outputs
   read as `''`, so `needs.X.outputs.skip != 'true'` is true and the job publishes from nothing.
   The result test is what makes `always()` safe; the two halves are one rule.
5. **Test the result for `== 'success'`.** A rule that only checks that `needs.X.result` is
   mentioned accepts `!= 'failure'`, and GitHub runs that on a skipped `X` (`result == 'skipped'`).
6. **A `uses:` job is a first-class `needs` node.** It takes a job-level `if:`, its `result` is
   `success` when the called workflow succeeded, and its `outputs` read normally downstream under
   `always()`.
7. **What the shape changes:** only cancelled runs. A publish on `always()` whose tested upstream
   had already succeeded still runs after a person cancels the run. `!cancelled()` would close
   that; sjawhar/legion's rule 6 accepts `always()` alone today (see *Not proven*).
8. **A `workflow_dispatch` does not reproduce a push's skipped job when the build jobs' own
   conditions name the trigger.** In `release.yaml` every `*_build` carries `github.event_name ==
   'workflow_dispatch'`, so on a dispatch every build runs and what skips is a `*_publish` whose
   build printed `skip=true`. Trace the trigger through the conditions before naming a job's
   conclusion as an acceptance criterion; name the property ("some job on the ancestor path is
   skipped and the publish succeeds") rather than the job.

## Evidence

- The defect: `Release Packages` run 38822866387 at sjawhar/legion merge `7b7496a0`, the first
  run on the split build/publish jobs (sjawhar/legion#1969). `legion_build` printed `Bumping
  legion-v13.4.0 -> legion-v13.4.1` and succeeded; `legion_publish` (`if:
  needs.legion_build.outputs.skip != 'true'`) was `skipped` because `envoy_plugin_publish`, four
  jobs upstream, skipped on an unchanged plugin. `worker_image_release_note` (`if:
  needs.worker_image.result == 'success' && needs.legion_publish.outputs.tag != ''`) skipped the
  same way. No `legion-v13.4.1` tag, release or attestation.
- Rules 1-4, each with a control, on sjawhar/legion-smoke run 38833347317 (branch
  `legion-779-condition-probe`, deleted): `root` → `skipped_upstream` (`if: false`) → `mid`
  (`always() && …`, success, outputs `skip=false`, `tag=v1`) → `probe_new` (`always() &&
  needs.mid.result == 'success' && needs.mid.outputs.skip != 'true'`) **success**, log `probe_new
  ran with skip=false tag=v1`; on the same `mid`: `control_old` (today's `legion_publish` shape)
  **skipped**, `control_success` (`success() && …`) **skipped**, `control_result_only`
  (`needs.mid.result == 'success' && …`, no status function) **skipped**, `control_two_needs`
  (today's note shape) **skipped**; `root` → `failing` (`exit 1`) → `hazard_always_without_result`
  (`always() && needs.failing.outputs.skip != 'true'`) **success**, log `skip read as ''`;
  `probe_always_with_result` (the result tested) **skipped**.
- Rules 2, 5 and 6 on run 38836147591 (branch `legion-779-tester-probe`, deleted; the tester's
  18-job chain carrying the branch's exact conditions): `control_old_far` skipped with the skipped
  job **two hops** upstream and `build` green; `hazard_ne_failure` (`always() &&
  needs.far_skip.result != 'failure' && needs.far_skip.outputs.skip != 'true'`) ran with
  `result=skipped`; `image` (a `uses:` call under `worker_image`'s condition) succeeded and `note`
  read `needs.image.result` and `needs.publish.outputs.tag`; `publish_noop` skipped on
  `skip=true`; `publish_offmain` skipped on a skipped `build` (`result == 'skipped'` is false);
  `build_bad_changes` skipped on a failed `changes` (the tightening `needs.changes.result ==
  'success'` adds to `legion_build`).
- The six conditions and the checker rule that pins them: sjawhar/legion#1975
  (`.github/workflows/release.yaml` `legion_build`, `legion_publish`, `worker_image`,
  `worker_image_release_note`, `envoy_plugin_publish`; `release-envoy-infra.yaml` `publish`;
  `.github/scripts/check-release-credentials.sh` rule 6).

## Not proven

- Cancelled runs were not probed; the exposure is reasoned from `always()`'s definition and is
  the one `pi_envoy_publish`, `pi_legion_publish` and the listener's `release_publish` already
  carried.
- That `!cancelled()`, `failure()` or `cancelled()` suppress the implicit `success()` is GitHub's
  documentation, not a measured row: the runs measured `always()` (suppresses) and `success()`
  (does not). A migration to `!cancelled()` needs its own probe arm first, and rule 6 widened to
  accept it (sjawhar/legion#1975, Fast-follow).
- Rule 6a checks that `needs.<X>.result` is tested, not what it is compared with (rule 5 above is
  the human's job today), and reads `needs['x'].outputs` but not `needs.x['outputs']`.
