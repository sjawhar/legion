---
title: "A registry move one constant governs is one commit, and its first run races its own copy job"
category: github
tags:
  - ci-images
  - ghcr
  - staged-rollout
date: 2026-11-04
status: active
module: .github
related_issues:
  - "cybermonklabs/legion#2734"
---

# A registry move one constant governs is one commit, and its first run races its own copy job

- Before splitting a CI-image change into this repository's usual two commits, check whether a gate
  forbids the split. `check-ci-images.sh` holds one `REGISTRY` constant and enforces it against
  `.github/ci-images.yaml`'s own `registry:` key **and** against every consumer reference, so a
  registry move cannot be staged: a commit that moves one side alone fails the tree gate. Take the
  one commit and say in the pull request body why the two-commit rule does not apply.
- Then expect the race that rule exists to prevent. Consumer jobs wait on `changes` alone while the
  `ci-images` job is still copying, and four workflows pull the package in runs with no copy job at
  all, so the first run is red on `manifest unknown` and that red is a step, not a defect.
- Once `check-ci-images.sh package` answers that the package holds every row, read the verdict of a
  run that started **after** the copy landed. Prefer the current head's own run to a re-run of the
  raced one: re-running the raced run judges a head nobody reviews.
- The package's visibility is GitHub's default and no re-run cures a private one, so read it
  (`gh api /orgs/<owner>/packages/container/<name> --jq '{visibility,repository:.repository.full_name}'`)
  as soon as the copy job has run; only a person can change it.

## Evidence

cybermonklabs/legion#2734 moved the twelve mirrored images to `ghcr.io/cybermonklabs/legion/ci` at
the digests `.github/ci-images.yaml` already held, in commit `e16388a8`.

- The single constant is `.github/scripts/check-ci-images.sh:201`; it refuses a differing
  `registry:` key at `:397` and a differing consumer reference at `:646`, over 37 references in 33
  files. The two-commit rule it collides with is in `AGENTS.md`. `ci.yaml` runs the copy and then
  `check-ci-images.sh package` in that order, inside one job.
- The first run, 41706021023 at `e13581e7b70f`, was red in 17 jobs; the test job's log reads
  `Error response from daemon: manifest unknown` on
  `sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11`. Its `ci-images` job's
  copy succeeded at 2026-11-03T19:47:58Z, after those jobs had already failed their pull at about
  19:45.
- Run 41708820283 at the next head, `421f876b94a7`, was the first to read the filled package: it
  failed two jobs only, each a real defect of the change, and the other fourteen raced lanes were
  green. No re-run of 41706021023 was made.
- The package read immediately after the copy answered
  `{"repository":"cybermonklabs/legion","visibility":"public"}`, so the one unrecoverable risk did
  not arise; `check-ci-images.sh package` then answered "12 row(s), 12 of them in the package".
