---
title: "A change confined to skills/ or docs still cuts a release, so decide the changelog fragment from the package's --patch-path list"
category: legion
tags:
  - changelog
  - release
  - skills
date: 2026-10-30
status: active
module: skills
related_issues:
  - "sjawhar/legion#2545"
---

# A change confined to skills/ or docs still cuts a release, so decide the changelog fragment from the package's --patch-path list

Extends docs/solutions/legion/worker-pane-shell-gotchas.md, which states the bump half: a change
under one of a package's `--patch-path` paths bumps at least the patch version whatever the pull
request title's type. The delta is what follows from that bump.

- Before you conclude "prose only, no changelog", grep your changed paths against the
  `--patch-path` arguments of each package's version recipe in `.github/workflows/release.yaml`.
  A path that appears there ships a release, and that package owes a changelog fragment.
- Decide it from the path list, never from whether runtime code changed. Nothing is red when you
  get it wrong: no check reads a pull request for a missing fragment.
- A skill that two packages ship raises a second question the repository has not settled. Ask the
  architect rather than inventing a rule.

## Evidence

sjawhar/legion#2545 changed `skills/legion-retro/references/learnings.md` and four other
instruction files, and no runtime code.

- `.github/workflows/release.yaml:125-128` lists `--patch-path skills/legion-retro/` among nine
  skill directories in `PI_LEGION_VERSION_ARGS`, and `.github/scripts/release-bump.sh:82-84` sets
  `bump="patch"` for a commit touching a patch path when the titles give no bump of their own. So
  the merge publishes a new `@sjawhar/pi-legion`, and #2545 added
  `packages/pi-legion/.changes/unreleased/pr2545-Fixed-20261030-055126.yaml`.
- Main's precedent is split, which is how the question gets missed. #2420
  ("docs(legion-controller): the controller's role prompt and takeover note say when each launch
  claims") changed `skills/legion-controller/` and added fragments for both `packages/pi-legion`
  and `packages/claude-envoy`. #2442 ("docs: correct the type-check command and five Legion skill
  instructions") changed four files under `skills/` and added no fragment at all.
- The open half, reported to #2545's architect rather than decided here: `packages/claude-envoy`
  ships the same skills through its generated `legion/` plugin and has a `.changie.yaml` of its
  own, so whether a skills change owes that package a fragment too is unsettled. No check enforces
  either answer, and the fold is unaffected by the choice.
