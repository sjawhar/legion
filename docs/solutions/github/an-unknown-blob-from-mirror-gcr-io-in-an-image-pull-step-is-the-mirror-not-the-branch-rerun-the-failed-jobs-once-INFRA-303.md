---
title: "An unknown blob from mirror.gcr.io in an image-pull step is the mirror, not the branch: re-run the failed jobs once"
category: github
tags:
  - github-actions
  - mirror-gcr-io
  - docker-pull
  - ci-triage
date: 2026-10-26
status: active
module: .github/workflows
related_issues:
  - "INFRA-303"
  - "sjawhar/legion#2221"
---

# An unknown blob from mirror.gcr.io in an image-pull step is the mirror, not the branch: re-run the failed jobs once

Extends docs/solutions/github/a-ci-red-in-initialize-containers-with-toomanyrequests-is-the-runners-docker-hub-window-not-the-branch-read-the-step-rerun-once-after-the-window-LEGION-632.md.

That document makes `mirror.gcr.io` the cure for Docker Hub's rate limit. The mirror can fail too,
and in a different way.

- **Read the failing step, not the job name.** When `test`, `dispatch` and `envoy-smoke` all fail
  in "Pull the NATS test container images", "Pull test container images" or "Initialize
  containers", and `docker pull mirror.gcr.io/library/<name>@sha256:…` prints `unknown blob` on
  every retry, no test ran. The mirror could not serve one layer of that digest.
- **Check main, then re-run once.** Confirm main's recent runs of the same workflow passed and the
  branch touches nothing that names the image. Then run `gh run rerun <run-id> --failed` for each
  red run, with no push and no merge of main. A merge of main is warranted only if main shows the
  same pull failure and later fixes it.
- **Record it and complete on the green re-run.** Put the cause and the re-run attempt numbers in
  the PR's CI line and the handoff. Push the handoff alone, with the `skip-checks: true` trailer, so
  the verdict stays on the code head the re-run made green.

## Evidence

On sjawhar/legion#2221, code head 26bb37f6 (a change confined to `packages/dispatch`), three
jobs failed at 05:12-05:18Z on 2026-10-26 pulling
`mirror.gcr.io/library/nats@sha256:2ae57dfba306…`, with layer `d4854e17c8c8` answering
`unknown blob` three times each: `test` (Tests run 40270870455), `dispatch` and `envoy-smoke`
(Legion Envoy and Contracts run 40270870435). Main's last six Tests runs, through 04:25Z, were
green. One `gh run rerun --failed` of each run passed on attempt 2, every required check green, with
no code change.
