---
title: "Cancel a closed or draft pull request's runs through a workflow-level group keyed by number, and never with a skipped required check"
category: github
tags:
  - ci
  - concurrency
  - github-actions
  - required-checks
  - drafts
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-600"
  - "sjawhar/legion#2356"
---

# Cancel a closed or draft pull request's runs through a workflow-level group keyed by number, and never with a skipped required check

- **Key every `pull_request` concurrency group on `github.event.pull_request.number`.** A merged
  pull request's `closed` event carries `github.ref` = the base branch, so a ref key misses the
  pull request and lands in main's group; where that group cancels in progress it cancels main's
  own run (Docs' Pages build here). Keep push and merge_group keys on `github.sha` (or the ref
  where main's runs must queue, as Docs does).
- **Never give an event whose jobs all skip (`closed`, `converted_to_draft`) to a workflow that
  posts required checks.** A job skipped by its `if:` reports `skipped`, which GitHub's
  required-check rule and the Legion daemon count as passing, and the newest check run of a name
  wins, so that run can turn a red head green. Cancel such a workflow from a separate one
  instead, which leaves it `cancelled` (fail closed).
- **To cancel another workflow's runs without a runner, copy
  `.github/workflows/tests-closed.yaml`:** a workflow-level `concurrency` with the target's group
  and `cancel-in-progress: true`, one job with `if: ${{ false }}`, `permissions: {}`. A run whose
  jobs all skip still joins its workflow's group when GitHub creates it, and the cancel lands
  then, before any runner. Concurrency groups are shared across workflows.
- **Put a group that a skipped run must cancel through at workflow level.** A job skipped by its
  own `if:` does not join a job-level group [INFERENCE: only the workflow-level path was observed
  here], and a job-level group named like its workflow's is a deadlock GitHub cancels, so delete
  the job-level copy. Under `workflow_call` the `github` context is the caller's: give the callee
  a key distinct from the caller's group and keep its cancel off for non-PR events.
- **To skip drafts:** add `ready_for_review` to the trigger's `types` (no `types` means opened,
  synchronize, reopened only) and gate each job, or the one job every other job needs, with
  `if: ${{ !github.event.pull_request.draft }}`. It is true on push, merge_group, workflow_call
  and workflow_dispatch. Wrap a leading `!` in `${{ }}` or quotes: bare, YAML reads it as a tag.
  On an image-building job, `check-image-trigger-paths.sh` admits this term only while every
  pull request trigger that runs the job lists `ready_for_review`, and admits
  `github.event.action != 'closed'`; any other event term fails the required `lint`.
- **State what `closed` cannot do:** a pull request that conflicts when it closes gets no
  `closed` run; `paths` are matched against the final diff; a running `docker/build-push-action`
  step takes up to about 100 s to stop; and a reopen within about a minute of the close can be
  cancelled by a `closed` run GitHub creates late (push or reopen again).
- **Prove it on scratch pull requests before the merge** (the branch plus one empty commit per
  head): draft, ready, two quick pushes, close while runs wait. Read the cancelled run's
  annotation ("Canceling since a higher priority waiting request for `<group>` exists") and the
  check-run ids, not the run list alone. Prove a `workflow_call` path with a push-triggered
  scratch caller under the real caller's group, and name the real call as the production check.
- **When a workflow's cancel or skip behaviour changes, grep `docs/solutions/`,
  `docs/kubernetes.md` and `AGENTS.md` for its file name and its old group or cancel wording**,
  and fix every note whose advice becomes wrong.

## Evidence

- sjawhar/legion#2356 (INFRA-600), code head 32dd5458. Before it, 72 h of sjawhar/legion runs held
  3,952 draft job minutes, 4,703 runner minutes after a pull request's close and 2,167 minutes of
  superseded Worker Image builds (dispatch://INFRA-600/artifact/ci-queue-waste-before-2026-10-27-md).
- Scratch #2380 at 20fa840c: `Tests (closed pull request)` run 40465747080, its job `skipped` with
  no runner, cancelled Tests run 40465690636 one second after its creation ("…tests-2380 exists").
  At 4773bb61 the same canceller as a job-level group on a real job also cancelled (run 40452204228)
  but waited in the runner queue after every close (run 40452283161).
- Scratch #2359: the older head's Worker Image build 40452158988 was cancelled the second the newer
  run was created ("…worker-image-2359 exists"). Scratch #2357: a running build took 97 s to stop
  after the close (run 40451907126).
- Scratch #2375 (tester): Envoy's `closed` run 40463967410, created 28 s after the close, cancelled
  the reopened run 40463925526.
- Review round 1: with `ready_for_review` removed from `worker-image.yaml`, the image-trigger check
  still passed at 20fa840c; f3ece5e9 makes it exit 1.
- Scratch caller on `scratch/infra-600-call` under `release-${{ github.ref }}`: runs 40451991938
  and 40451999322 each built `success`, neither cancelled.
- Four notes described Worker Image's old serialise-per-PR behaviour and had to be rewritten:
  the LEGION-767, INFRA-255 and LEGION-631 notes and `pull-request-trigger-paths-follow-the-pr-head.md`.
