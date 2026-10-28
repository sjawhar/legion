---
title: "A file name from data reaches git as a literal path, and its first add is found across renames, never with git log --follow"
category: best-practices
tags:
  - git
  - pathspec
  - literal-pathspecs
  - renames
  - log-follow
  - shell
date: 2026-10-28
status: active
module: .github/scripts
related_issues:
  - "INFRA-629"
  - "sjawhar/legion#2419"
---

# A file name from data reaches git as a literal path, and its first add is found across renames, never with git log --follow

## The rule

1. When a script gives git a path that it did not write itself (a name from `git ls-tree`, a
   directory listing, user input or a fragment someone committed), run git with
   `git --literal-pathspecs …` (or the `:(literal)` prefix) on every call that takes a pathspec:
   `log`, `diff`, `rm`, `add`, `ls-files`. Without it, `[`, `*`, `?` and a leading `:` in the
   name are pathspec magic, and the call silently reads other files. A path you restrict by a
   regex to letters, digits, `.`, `_`, `-` and `/` needs no flag; say so in a comment beside the
   regex.
2. Test it with two files whose names one reads as a glob of the other, such as `X-[a].yaml` and
   `X-a.yaml`, each added in a different commit.
3. To find the commit that first added a file, follow renames yourself: run
   `git --literal-pathspecs log -1 --diff-filter=A --format=%H <rev> -- <path>`; if
   `git diff-tree -r -z -M --root --no-commit-id --name-status <commit>` shows that commit renamed
   another file to `<path>`, repeat from `<commit>^` with the old name. Never use
   `git log --follow` for this: it also follows copies, found against every file of the parent
   commit, so a new file that is alike enough to an older one (small YAML files of the same
   shape) gets the older file's commit.
4. `--diff-filter=A` alone gives the commit that gave the file its current name, so a file
   renamed later is dated by the rename. Test a `git mv` after the first add, and a new file that
   is alike to an older one and never renamed.

## Evidence

- `release-push.sh fold-plan` labels a changelog fragment with the first release that contains
  the commit that added it. Its `git log -- "$path"` read `INFRA-1-[a].yaml` as a pattern and
  dated it by the commit that added `INFRA-1-a.yaml`, so it got the later release (the tester's
  red test, `release-push.test.sh`; fixed in 0bcd639c45ab with `--literal-pathspecs`).
- Review round 2 found that a fragment renamed after its release got the next release, because
  `--diff-filter=A` does not follow renames. The reviewer suggested `--follow`. Measured on git
  2.47.3, `--follow` reported `C050 a.yaml → g[x]*.yaml` at the commit that added a new fragment,
  so a never-renamed fragment took an older fragment's commit. The fix is `first_added` in
  `release-push.sh` (460a6f9e021b), which follows `diff-tree -M` renames commit by commit and
  never copies.
- Known limit of the rename walk: one commit that deletes a file and adds a different but alike
  one pairs them as a rename. A `log.follow=true` setting turns a single-path `git log` into
  `--follow`, so a script that runs where it may be set passes `--no-follow` (git 2.47.3 accepts
  it); `first_added` does not pass it yet, and its tests run with `GIT_CONFIG_GLOBAL=/dev/null`.
