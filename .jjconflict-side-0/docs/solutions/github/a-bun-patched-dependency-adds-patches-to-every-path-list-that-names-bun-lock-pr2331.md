---
title: "A Bun patched dependency adds patches/ to every path list that names bun.lock"
category: github
tags:
  - bun
  - patchedDependencies
  - ci-path-filters
  - release
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2331"
---

# A Bun patched dependency adds patches/ to every path list that names bun.lock

Extends `docs/solutions/github/a-shared-go-package-or-helper-script-joins-every-dockerfile-copy-and-ci-path-list-that-builds-or-runs-it-INFRA-253.md`.

- `bun.lock` names a patch only by its path. An edit to `patches/<pkg>@<ver>.patch` changes no
  other file, so a job filtered on `bun.lock` alone never runs for it.
- When you add a `patchedDependencies` entry:
  - Add `patches/**` to every CI path filter that lists `bun.lock` for a job that runs, bundles or
    checks the patched package.
  - Add `patches/` to the release version paths of every package whose shipped bundle inlines it,
    as a patch path, so a patch-only commit releases it.
- Bun ignores a `patchedDependencies` entry for a version that is not installed, and says nothing.
  Moving the dependency to another version means moving the patch with it.

## Evidence

- Review round 1, thread PRRT_kwDORFy7ds6wkMTU: a change to `patches/nats@2.29.3.patch` alone ran
  none of the stall tests, no claude-envoy bundle check and no pi-envoy release.
- 0504cbfb added `patches/**` to the `envoy_client`, `envoy_e2e_local`, `envoy_session_stall`,
  `claude_plugin`, `pi_envoy` and `pi_legion` filters in `.github/workflows/envoy-and-contracts.yaml`.
- The release side is `release.yaml`'s `PI_ENVOY_VERSION_ARGS` (`--patch-path patches/`). It was
  carried into main's restructured file at merge 5384eb84.
