---
title: "A Path-Filtered Check Must Trigger on pull_request, Not push"
category: github
tags:
  - github-actions
  - workflow-triggers
  - path-filters
  - reusable-workflows
  - permissions
date: 2026-09-12
status: active
module: ci
related_issues:
  - "sjawhar/legion#966"
symptoms:
  - "a required or expected check is missing from a PR head after a small follow-up commit"
  - "a by-design red check silently stops guarding a PR once a docs- or handoff-only commit lands"
  - "a reusable workflow fails with a permissions error although the caller's top-level permissions look sufficient"
---

# A Path-Filtered Check Must Trigger on pull_request, Not push

## Symptom

`worker-image.yaml` first used a `push` trigger with `branches-ignore: [main]` and three `paths`
(the Dockerfile directory, the pin module, the workflow file). It fired for the commit that added
those files, and its failing `build` check attached to that head. The next commit on the PR was a
handoff-only `.legion/implement.json` change: the `push` event's paths did not match, the workflow
did not run, and the PR head had **no** Worker Image check at all. The red that was supposed to
guard the merge had detached from the head nobody would look at twice.

## Mechanism

GitHub evaluates `on.push.paths` against the files changed **in the pushed commits**. It evaluates
`on.pull_request.paths` against the **whole PR diff** (base...head). A PR whose diff touches an
image file therefore gets the check on every synchronize event under `pull_request`, but only on
the pushes that themselves touched those paths under `push`.

Two consequences for the built artifact under `pull_request`:

- `github.sha` is the ephemeral merge commit, not the PR head. Anything that names the built
  commit (a `sha-<short>` tag, an OCI `revision` label, `actions/checkout`) must use
  `github.event.pull_request.head.sha`, falling back to `github.sha` for `workflow_call` and
  `workflow_dispatch`:
  `BUILT_SHA: ${{ github.event.pull_request.head.sha || github.sha }}`.
- Concurrency should key on the PR, not the ref: `group: ${{ github.event.pull_request.number ||
  github.ref }}` serialises two pushes to one PR and leaves other events on the ref key.

Same-repo `pull_request` runs receive the workflow's declared `permissions` (here `contents`,
`packages`, `id-token: write`); a fork PR would get a read-only token. Say which you rely on.

## Fix

```yaml
on:
  pull_request:
    branches: [main]
    types: [opened, synchronize, reopened]
    paths:
      - packages/daemon/docker/**
      - packages/daemon/src/daemon/omp-pin.ts
      - .github/workflows/worker-image.yaml
```

with `ref: ${{ env.BUILT_SHA }}` on checkout and `BUILT_SHA` feeding the tag and label. A
`workflow_dispatch` trigger works only once the workflow file exists on the default branch, so it
is the post-merge path, never the pre-merge one.

## Reusable-workflow caller permissions and secrets

When `release.yaml` calls `worker-image.yaml` with `uses:`, the **caller job's** permissions must
cover everything the callee declares — job-level `permissions:` on the calling job replace the
workflow-level set for that job, so put the full set there (`contents: write`, `packages: write`,
`id-token: write`) rather than widening the top-level block for every job in the file.

`secrets: inherit` is not needed when the callee reads only `secrets.GITHUB_TOKEN`: a called
workflow receives `GITHUB_TOKEN` automatically. `inherit` hands the callee every repository secret
(here `DEPLOY_KEY`) — for a job that builds a PR-authored Dockerfile, that is a needless grant.
Declare `secrets:` explicitly or omit it.

## Rerun the failed jobs, not the run

The remedy text for a failed image build must say `gh run rerun <run-id> --failed`. A whole-run
rerun of a `release.yaml` invocation re-executes the `cli` job at the same commit, which now sees
its own `cli-vX` tag, computes an empty bump range, skips, and hands the image job an empty
`cli_version` — the retried image publishes `sha-` only, with no version tag and no release-body
append. `--failed` keeps the succeeded jobs' recorded outputs.
