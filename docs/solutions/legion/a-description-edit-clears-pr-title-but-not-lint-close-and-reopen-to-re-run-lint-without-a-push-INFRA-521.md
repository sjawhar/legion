---
title: "A description edit clears PR Title but not lint: close and reopen to re-run lint without a push"
category: legion
tags:
  - public-repository
  - check-pr-text
  - pr-body
  - github-actions
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-521"
  - "sjawhar/legion#2313"
---

# A description edit clears PR Title but not lint: close and reopen to re-run lint without a push

Extends docs/solutions/legion/the-private-names-check-reads-each-events-frozen-body-fix-the-text-then-push-and-lint-a-handoff-before-its-trailered-commit-LEGION-781.md.

- The required `lint` job in the `Tests` workflow runs `check-pr-text.sh` only on
  `pull_request` `opened`, `reopened` and `synchronize`. A description edit starts only `PR Title`
  (`pr-title.yaml`, which also listens to `edited`). So after fixing description text, `PR Title`
  goes green and `lint` stays red at that head.
- To re-run `lint` with the corrected text when there is nothing to push (a handoff-only push
  carries `skip-checks: true` and starts nothing), close and reopen the pull request at the same
  head. The `reopened` event carries the current body and a fresh merge ref.
- A reopen also starts every other `pull_request` workflow. Cancel the runs that already passed at
  that head, but only those no branch rule requires.
- A job re-run replays the old event payload and the old merge commit, so it also cannot pick up a
  fix that landed on the base since the run started.

## Evidence

sjawhar/legion#2313: the first description named a private host, and was corrected 21 minutes
later. The `Tests` run at 357138e7 (40441476636) still failed `lint` on `body:20`, and its `test`
failed on a base defect. Main had since fixed that defect (baadccdc). A close and reopen at 05:00Z
started `Tests` 40452900509 on merge ref ce8d5826, where `lint` and `pr-title` passed. Its
duplicate `Legion Envoy and Contracts` and `Docs` runs were cancelled.
