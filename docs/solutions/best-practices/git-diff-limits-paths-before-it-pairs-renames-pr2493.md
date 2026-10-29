---
title: "git diff limits paths before it pairs renames"
category: best-practices
tags:
  - git
  - pathspec
  - renames
  - change-detection
date: 2026-10-29
status: active
module: .github/scripts
---

# git diff limits paths before it pairs renames

## The rule

1. `git diff --name-only <a> <b> -- <pathspecs>` applies the pathspecs before it detects renames.
   A file renamed out of the set is already listed by its old path, with or without
   `--no-renames`.
2. `--no-renames` changes the result only for a rename whose old and new paths are both inside the
   set: without it, git lists only the new path. Pass `--no-renames` whenever every touched path
   must be seen, whatever the user's `diff.renames` setting is.
3. The discriminating test renames a file inside the set. A rename out of the set passes with or
   without the flag, so it proves nothing.
4. Pathspecs are relative to the current directory. A script that reads repository paths changes to
   the repository root before its first pathspec call, and makes any path argument from the caller
   absolute before it changes directory. From a subdirectory the read silently matches nothing, and
   an empty diff reads as "nothing changed".

## Evidence

- sjawhar/legion#2493: `build_inputs_changed` runs
  `git diff -z --no-renames --name-only "$base" "$tip" -- "${pathspecs[@]}"`
  (`.github/scripts/release-push.sh:704`). `.github/scripts/release-push.test.sh:1541` states rules
  1-3 above the rename-inside case. With `--no-renames` removed, only that case failed (614 passed,
  1 failed at d7b3cc91; 622 passed, 1 failed at af843c86). The rename-out-of-the-plugin case kept
  passing.
- Review thread PRRT_kwDORFy7ds6xe-nZ, fixed in 2d7200de83ad: push-built read root-relative
  pathspecs before moving to the root. Disabling the move fails the subdirectory newer-build case
  (621 passed, 2 failed). The subdirectory landing case passes either way, because a later helper
  moves to the root on its own.
- Boundary with docs/solutions/best-practices/a-required-check-runs-mains-post-merge-command-and-writes-nothing-in-the-checkout-pr2423.md,
  rule 4: `git rev-parse --show-toplevel` fails in a jj workspace with no colocated git. Use it, as
  push-built does, only in a command that runs in a CI checkout. A command a worker also runs in a
  jj workspace finds the root from its own path.
