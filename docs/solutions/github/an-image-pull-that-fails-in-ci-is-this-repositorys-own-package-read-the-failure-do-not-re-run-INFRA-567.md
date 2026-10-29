---
title: "An image pull that fails in CI is this repository's own package: read the failure, do not re-run"
category: github
tags:
  - github-actions
  - container-images
  - ghcr
  - docker-pull
  - ci-triage
date: 2026-10-29
status: active
module: .github/workflows
related_issues:
  - "INFRA-567"
  - "INFRA-303"
  - "LEGION-632"
  - "sjawhar/legion#2512"
---

# An image pull that fails in CI is this repository's own package: read the failure, do not re-run

Replaces two pages this change retired. Both told a reader to re-run the failed jobs after an
image pull failed: "A CI red in Initialize containers with toomanyrequests is the runners' Docker
Hub window, not the branch" (LEGION-632) and "An unknown blob from mirror.gcr.io in an image-pull
step is the mirror, not the branch" (INFRA-303). Their measurements are the evidence for this
change and are kept under Evidence below. Their advice is wrong now. No CI job of this repository
reads a third-party registry any more, so a failed pull is this repository's own fault.

## What was measured

- **Two red windows in 17 days, neither caused by the branch.** On 2026-10-09 the GitHub-hosted
  runners' shared egress address reached Docker Hub's anonymous pull rate limit, and every job
  that pulls an image went red with `toomanyrequests`. On 2026-10-26 Google's mirror answered
  `unknown blob` for one layer of a digest three times in a row, and three jobs went red. A third
  reading of the same fault came from this change's own first mirror run, which failed part way
  with `BLOB_UNKNOWN` from the mirror.
- **The cost is a re-run of every job that pulls, plus the wait for someone else's registry.** A
  red image pull runs no test of that job, so the job's work is lost and the run is red. The
  2026-10-09 window ran from the first red at 21:01Z to main's fix at 22:48Z, 1 hour 47 minutes,
  over three pushes that each went red the same way, with one failed re-run inside the limit's own
  window. Each re-run of a rate-limited pull also counts against that window, so a re-run makes
  the hold longer for every other pull request on the same runners.
- **Retrying inside the step did not help either.** The pull steps retried, and each retry
  answered the same fault. On 2026-10-26 the one layer answered `unknown blob` three times in
  each failed step.

## What replaced the re-run

- Every image a CI job fetches is a copy in one package this repository publishes,
  `ghcr.io/sjawhar/legion/ci`. Each consumer names its copy by digest, so a push can add bytes to
  the package but can never change the bytes a job pulls.
- `.github/ci-images.yaml` holds one row per image (`upstream`, `tag`, `digest`).
  `.github/workflows/mirror-images.yaml` publishes the rows the package does not hold yet, with
  `crane copy`, which keeps the manifest digest. Adding an image, or moving one to a new version,
  is one edit of that file.
- `.github/scripts/check-ci-images.sh` is the gate. It refuses a reference in CI's reach that
  names anything but a digest that file holds, and it names each file outside CI's reach that
  still names `mirror.gcr.io`, with the one ground that admits it.
- Both three-attempt `docker pull` retry loops are gone: the one in
  `.github/actions/pull-test-images/action.yml`, and the one in `ci.yaml`'s step that starts the
  file store. The registry is this repository's own, so a failed pull must be seen.

## What to do when an image pull fails now

1. Read the failing step and the reference it names. It names `ghcr.io/sjawhar/legion/ci` and a
   digest.
2. Do not re-run the job, and do not add a retry. The answer came from this repository's own
   package, and a second request gets the same answer.
3. Read the failure for which of three things it is:
   - The package does not hold that digest. A row was added or edited, and the Mirror Images run
     for that commit did not copy it. Read that run. It fails loudly on a copy that lands at
     another digest.
   - The registry refused the read. The package is public, and a public package answers an
     anonymous pull, so a `403` means the package's visibility changed. That is an act by a
     person, not a branch's doing.
   - The reference is not a row of `.github/ci-images.yaml` at all. Then
     `.github/scripts/check-ci-images.sh` would have refused it, so read why the gate did not run.

## Evidence

- **LEGION-632, sjawhar/legion#1842, 2026-10-09 21:01–21:33Z.** The retro's docs commit cd8f3d20,
  the `.legion/` removal 6cc0c109 and the harness fix bb12c138 each turned `Tests` (pgvector's
  pull in `Initialize containers`), `Worker Image` (`debian:bookworm-slim` in buildx, then
  `moby/buildkit` in `setup-buildx-action`), `Docs` and seven container jobs of `Legion Envoy and
  Contracts` red with `toomanyrequests`. `lint`, `typecheck`, `pr-title` and the plugin test jobs
  passed each time, since they pull nothing. Attempt 2 of `Tests` at 21:07Z failed the same way.
  Main's #1871 landed at 22:48Z, and the forward merge 3366e977 brought it. `Tests` 38008011356
  and `Docs` 38008011222 at the next head were green on the first run.
- **INFRA-303, sjawhar/legion#2221, 2026-10-26 05:12–05:18Z.** At code head 26bb37f6, a change
  confined to `packages/dispatch`, three jobs failed pulling
  `mirror.gcr.io/library/nats@sha256:2ae57dfba306…`, with layer `d4854e17c8c8` answering
  `unknown blob` three times each: `test` (Tests run 40270870455), `dispatch` and `envoy-smoke`
  (Legion Envoy and Contracts run 40270870435). Main's last six Tests runs, through 04:25Z, were
  green. One `gh run rerun --failed` of each run passed on attempt 2, with no code change.
- **INFRA-567, sjawhar/legion#2512, 2026-10-29.** The first Mirror Images run of this change
  failed part way with `BLOB_UNKNOWN` from `mirror.gcr.io`, so each row's `upstream` is now the
  image's own Docker Hub reference. The 12 digests are the same at both hosts, read at both. At
  17:1xZ, from a pod holding no credential, an anonymous token request
  (`GET https://ghcr.io/token?scope=repository:sjawhar/legion/ci:pull&service=ghcr.io`) and one
  manifest `HEAD` per row with that token answered `200` for all 12 rows. A `HEAD` with no token
  answers `401`, which is the registry asking for a token rather than refusing the package. GitHub
  published the package with the repository's own visibility, so no person had to act, no
  consuming job needs a `docker/login-action` step, and no service container needs a
  `credentials:` block.
