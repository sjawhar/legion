---
title: "A change that retires a path or a mechanism greps the old string repository-wide, not the files the diff touches"
category: legion
tags:
  - census
  - documentation
  - retired-paths
  - worker-image
date: 2026-10-16
status: active
module: docs/kubernetes.md, packages/daemon/docker/worker.Dockerfile
related_issues:
  - "LEGION-636"
  - "sjawhar/legion#1984"
---

# A change that retires a path or a mechanism greps the old string repository-wide, not the files the diff touches

Extends docs/solutions/legion/a-review-fix-that-deletes-code-greps-the-prose-the-pull-request-added-for-the-deleted-name-and-writes-a-librarys-behaviour-from-its-source-not-from-the-absence-of-your-own-handler-LEGION-695.md.

- That document greps the prose the pull request itself added. The inverse survives a diff
  review: prose nobody touched, sixty lines above a hunk, still describing the retired path or
  mechanism. The census of such a change has one row per retired string, grepped over the whole
  repository, with the hits that are expected to survive named:
  `grep -rn '/opt/node/lib/node_modules\|npm install -g' docs packages scripts`.
- Run it at the end of implementing, over the tree rather than the diff, and once more after
  each review round; a file the diff updates in four places can still hold the old statement in
  a fifth.

## Evidence (LEGION-636, sjawhar/legion#1984)

The language servers moved from `npm install -g` into `/opt/node/lib/node_modules` to
`npm ci --ignore-scripts` into `/opt/language-servers/node_modules`. The diff updated the
Dockerfile's comments, the notices text and four passages of `docs/kubernetes.md`; the image
inventory paragraph at `docs/kubernetes.md:78-81`, outside every hunk, still said "npm global
packages under `/opt/node/lib/node_modules`". The PR body's census had rows for `installed`,
`model-fallback=`, `DaemonAPIVersion` and the signatures, and none for the two retired strings;
the reviewer's thread PRRT_kwDORFy7ds6tDz4D found the paragraph, and the grep above found no
other survivor after the fix.
