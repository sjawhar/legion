---
title: "One job that publishes several packages resets to the run's commit before each one, and a rerun proves it"
category: github
tags:
  - github-actions
  - release-workflow
  - release-push
  - version-bump
  - rerun
date: 2026-10-28
status: active
module: .github/scripts
related_issues:
  - "INFRA-576"
  - "sjawhar/legion#2358"
---

# One job that publishes several packages resets to the run's commit before each one, and a rerun proves it

## The rule

1. When one job runs `release-push.sh tag` and `push-main` for several packages in turn, start
   each package with `git checkout --force --detach "$GITHUB_SHA"` and `git clean -ffdx`, so
   each bump commit is one commit on the run's commit (`tag` commits on HEAD, `tagged` finds a tag
   only on HEAD or its child, `push-main` refuses a HEAD more than one commit ahead of main). Tag
   a package that commits nothing with the commit explicitly (`git tag "$TAG" "$GITHUB_SHA"`).
2. Prove the composition with a rerun, not only a first attempt. A first attempt passes without
   the reset, because each `push-main` lands the previous bump on main. On a rerun `push-main`
   pushes nothing, so the earlier package's bump stays on HEAD and the next package's `push-main`
   refuses.
3. Two bumps of different packages change different `package.json` files, so `push-main`'s rebase
   of the second onto the first has nothing to resolve: the packages need no serial jobs.

## Evidence

- `.github/workflows/release.yaml` lines 509-518 (the publish job's comment) and the resets at
  580, 671, 823 and 916; legion's explicit tag in "legion: Tag release".
- `.github/scripts/release-push.test.sh`, case "one publish job releases two packages from the
  run's commit": both bumps land, a rerun releases each tag again and pushes nothing, and the
  control without the reset ends in `HEAD carries 2 commits main does not`. A first attempt
  without the reset passed, which is why the control is a rerun.
- `docs/solutions/github/release-bump-push-resolves-one-conflict-shape-and-routes-every-failure-through-one-sink.md`
  covers reruns of one package.
