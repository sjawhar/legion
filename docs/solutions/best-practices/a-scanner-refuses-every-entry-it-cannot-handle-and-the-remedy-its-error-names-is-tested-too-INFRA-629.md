---
title: "A scanner refuses every entry it cannot handle, and the remedy its error names is tested too"
category: best-practices
tags:
  - shell
  - refusal
  - silent-skip
  - error-messages
  - branch-names
date: 2026-10-28
status: active
module: .github/scripts
related_issues:
  - "INFRA-629"
  - "sjawhar/legion#2419"
---

# A scanner refuses every entry it cannot handle, and the remedy its error names is tested too

## The rule

1. A script that walks a directory of inputs (fragments, manifests, migrations) never `continue`s
   past an entry it does not handle: a subdirectory, a symlink, another extension. It fails once,
   naming every such entry, sorted. A skipped input is lost with nobody told.
2. List the directory without recursion (`git ls-tree` without `-r`, `find -maxdepth 1`), so a
   subdirectory shows as one entry to refuse, not as files to read.
3. Drive the remedy the error message names (rename it, move it) as a test case. A remedy applied
   after the input already shipped can break what the script derives from history.
4. An instruction that builds a file name from a branch name says how to write `/`: a branch name
   such as `owner/fix-thing` makes a subdirectory.

## Evidence

- Review round 1 of sjawhar/legion#2419: `release-push.sh fold-plan` skipped every entry of
  `.changes/unreleased/` that was not a regular `*.yaml` file, so `unreleased/sjawhar/fix-thing.yaml`
  and `INFRA-1-a.yml` stayed on main for ever with exit 0. The root `AGENTS.md` told authors to use
  the branch name in place of the issue key, which makes such a subdirectory. Fixed in
  6b255451bb62: fold-plan refuses them by name, and `AGENTS.md` then said to write each `/` as
  `-`. `AGENTS.md` now names a fragment by the pull request number, so no branch name reaches a
  fragment's path.
- Review round 2: the refusal told the author to rename the entry. No pull-request check reads
  fragments, so the entry reaches main, and its release is cut, before anyone sees the refusal; a
  later rename then dated the fragment by the rename commit and gave it the next release. Fixed in
  460a6f9e021b (see
  `docs/solutions/best-practices/a-file-name-from-data-reaches-git-as-a-literal-path-and-its-first-add-is-found-across-renames-never-with-log-follow-INFRA-629.md`),
  and the refusal now says to rename in one commit with `git mv`.
