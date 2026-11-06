---
title: "A base deletion leaves false prose in the files your branch touches: grep them at the merge"
category: github
tags:
  - forward-merge
  - conflict
  - workflows
  - documentation
date: 2026-10-29
status: active
module: .github/workflows
---

# A base deletion leaves false prose in the files your branch touches: grep them at the merge

Extends docs/solutions/github/a-branch-that-deletes-files-main-still-edits-ports-each-edit-at-every-forward-merge-and-reruns-its-coverage-checks-on-the-merged-tree-pr2316.md,
from the side of the branch whose edit the deletion met.

## The rule

1. When a forward merge takes a file the base deleted or replaced, the merge reports a conflict in
   that file alone. Every other mention of its path merges clean and is now false: comments in
   scripts, file headers, guides, and your own earlier `docs/solutions/` learnings.
2. In the merge commit, grep the deleted path's name, and the names of any other files the same
   base change deleted, across the files your branch adds or edits
   (`git diff --name-only <fork point> <merge>`). Fix every hit to name the replacement.
3. Keep the search to your own files. Older learnings on the base that name the deleted path are
   dated records, not your merge's to rewrite.
4. Nothing else catches this. The conflict marks only the deleted file, coverage checks such as
   `check-ci-filters.sh` read filters and commands rather than comments, and the unchanged-diff
   fingerprint leaves out `docs/solutions`. A green filter check is no proof that a moved non-Go
   step is covered either: cite the filter anchor that already names what the step reads.

## Evidence

- sjawhar/legion#2493, forward merge af843c867d99 onto #2316, which replaced pr-and-main.yaml,
  docs.yaml and envoy-and-contracts.yaml with `.github/workflows/ci.yaml`. The only conflict was
  `pr-and-main.yaml`. The implementer moved this branch's step into ci.yaml's `test` job and fixed
  the one learning that named the deleted file.
- The merge missed `packages/claude-envoy/scripts/verify-build.sh:7-8` and
  `.github/scripts/release-push.sh:188`, which still named `pr-and-main.yaml` and
  `envoy-and-contracts.yaml`. The reviewer fixed both a commit later, in ba5200ac36d1.
- The moved `bun test tests/build-inputs.test.ts` step is covered because ci.yaml's `test` filter
  includes the `claude_envoy` anchor (`ci.yaml:410`, `:443`). The filter check could not have
  failed on it, since it reasons about go commands and the step is not one.
