---
title: "A renamed package seeds its first release from the old name's npm version, and its trusted publisher is added inside npm's two-day window"
category: github
tags:
  - release-workflow
  - npm
  - trusted-publishing
  - package-rename
  - release-bump
  - version-seed
date: 2026-10-07
status: active
module: .github/workflows/release.yaml
applies_when:
  - A package is renamed or split and the release workflow gains a job with a new tag prefix
  - The new npm name exists only as a placeholder publish
  - npm trusted publishing is being configured for a name that has never published from CI
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A renamed package seeds its first release from the old name's npm version

- When no tag with the new prefix exists, seed `PREV_VERSION` from `npm view <old name> version`,
  never from the new name: the new name on npm answers the placeholder `0.0.0` until the first
  real publish, which would restart the line at `0.x`. The first tag retires the seed branch on
  its own; write the reason beside the seed, and allowlist the old name there in any check that
  forbids it elsewhere.
- Each `--patch-path` the bump script takes must also appear after `--` (the script reads a
  patch path only when it is also a tracked path); dry-run the exact arguments with
  `release-bump.sh <seed> HEAD …` in a checkout before relying on them.
- A second job that also ends in a push to main waits on the first (`needs:` with the
  `always() && (success || skipped)` chain): the push's race repair covers one package.
- Trusted publishing is per package name, and npm drops a configuration that has not completed a
  publish within two days. Have the placeholder published early so the name and its settings
  page exist, and open the trusted-publisher ask only once review is done, with the merge to
  follow inside the window; a release that runs before it fails at `npm publish` and is re-run
  with `workflow_dispatch`, which the job's `npm view` skip makes safe.
- The committed manifest version is not the released version. Readers of the diff see the last
  pre-split version in both manifests; the release job computes the next one.

## Evidence

sjawhar/legion#1831, `.github/workflows/release.yaml`: `pi_envoy` (tag `pi-envoy-v*`) and
`pi_legion` (tag `pi-legion-v*`, `needs: [changes, pi_envoy]`) each seed from
`npm view @sjawhar/pi-legion-envoy version` when no tag exists. At the head both committed
manifests read 7.11.2 (main's last pre-split release, taken at the second forward merge); the
tester ran `release-bump.sh` with each job's exact arguments and got `8.0.0` for both, the
breaking marker on the squash subject supplying the major bump, with `npm view` of each new name
answering `0.0.0`. The placeholder publish was the architect's ask answered before the merge
(dispatch://LEGION-247/ask/8d63dcf6-ab14-47fc-9d23-05bbe4895f00); the trusted-publisher ask opens
at the review's approval for that reason.
