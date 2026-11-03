---
title: "A step the base adds runs against your head's tree when the job checks out the head sha"
category: legion
tags:
  - ci
  - merge-ref
  - forward-merge
  - github-actions
date: 2026-11-03
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2688"
---

# A step the base adds runs against your head's tree when the job checks out the head sha

Extends docs/solutions/legion/a-red-check-in-a-package-the-branch-does-not-touch-is-mains-until-mains-own-run-says-otherwise-pr2580.md.

That page's step 3 reads the base's own push run for the failing job and splits on it: red on the
base means the cause is the base's, green on the base means "the red is yours or a flake. Re-run
the failed jobs once." This case is the exception. The base is green by construction, the red is
nobody's code, and the re-run cannot pass.

- **A `pull_request` run resolves the workflow file from the merge ref, while a job that sets
  `ref:` to the head sha checks out your unmerged tree.** A step the base added since your fork
  point then runs against a tree that never had the file it calls, and the job fails with
  `No such file or directory` and exit 127 naming a path, not with an assertion.
- **Scope it by the job's `ref:`, not by the job's name.** `actions/checkout` defaults to the
  merge ref, so almost every job gets the base's new step and the base's new file together and is
  unaffected. Only a job that overrides `ref:` to the head is exposed. Read the failing job's
  `with.ref:` in the workflow as the merge ref holds it; that expression is the whole rule.
- **A missing-file error with the base green is this case, and the base's push run is not the
  discriminator.** Pair the symptom with two reads of the file itself:
  `gh api repos/<owner>/<repo>/contents/<path>?ref=<your head>` answering 404 while `?ref=<base>`
  answers its bytes. An ancestry check (`jj log -r '<the base commit> & ::<your head>'` empty)
  confirms the base commit is not yours, but says nothing about the tree-versus-workflow split, so
  it is a cheap second opinion and not the mechanism.
- **Do not re-run the job.** The re-run replays the same merge ref *and* the same head sha, so it
  fails again and spends a fix attempt. The page above forbids a re-run only for the red-on-base
  case; this is the other one.
- **Clear it with one forward merge of the base, now.** There is nothing to wait for: the file is
  already on the base. Merge, push, re-take the change's own proof at the merge head, and record
  the fingerprint pair, as that page's step 5 says.

Two sibling pages hold the halves of the mechanism, each stating it as a benefit rather than a
hazard: `docs/solutions/github/a-worker-image-path-filter-covers-the-code-the-init-container-runs.md`
(the workflow comes from the merge ref, so a trigger a pull request widens applies to itself) and
`docs/solutions/github/pull-request-trigger-paths-follow-the-pr-head.md` (the build checks out the
head sha, so the image it tags names the commit a person pushed). The boundary is
`docs/solutions/github/a-base-gate-arrives-in-the-merge-and-the-job-your-branch-alone-adds-is-the-line-it-refuses-pr2517.md`:
a gate that arrives in the merge and judges the merged tree is judging your change, and its red is
yours.

## Evidence

In sjawhar/legion#2688, `worker-image / build` failed at head `bf78db38` (run 41579696373, job
125741987773) in its step `Derive the image repository`:
`.github/scripts/repo-paths.sh: No such file or directory`, exit 127. Main's #2648 had added both
that step and that script at `f5bdbadaca14`, committed after the branch's fork point, and
`jj log -r 'f5bdbada & ::bf78db38'` is empty. `gh api
repos/sjawhar/legion/contents/.github/scripts/repo-paths.sh?ref=bf78db38` answered 404 while the
same read at `ref=main` answered 3,916 bytes.

The job is exposed because it alone overrides the checkout:
`.github/workflows/worker-image.yaml` sets `BUILT_SHA: ${{ github.event.pull_request.head.sha ||
github.sha }}` and `ref: ${{ env.BUILT_SHA }}`, deliberately, so the image's `sha-` tag names the
commit a person pushed. The same script runs in `lint` and `test` through `ci.yaml`, which take
the default merge-ref checkout, and neither failed. `worker-image` is in `ci-ok`'s `needs:` and in
its `PULL_REQUEST_ONLY` list, and `ci-ok` is required on main, so the head could not merge.

The remedy was one merge commit of `main@origin` at `da8cb799` into the issue branch,
`a07f5ac46f8c`, with no conflict and nothing else in it; the head's tree then carried the script,
and the unchanged-diff fingerprint was `ff694d3c1150cda3…` either side of the merge, so the
reviewer's approval stood and the round was a confirmation.
