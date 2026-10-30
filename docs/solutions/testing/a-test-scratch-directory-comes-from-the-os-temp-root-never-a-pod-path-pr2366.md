---
title: "A test's scratch directory comes from the OS temp root, never from a pod path"
category: testing
tags:
  - tmpdir
  - ci
  - github-actions
  - worker-pod
date: 2026-10-28
status: active
module: scripts/dispatch
related_issues:
  - "sjawhar/legion#2366"
---

# A test's scratch directory comes from the OS temp root, never from a pod path

- Make a test's scratch directory with `mkdtemp(join(tmpdir(), "<name>-"))`, where `tmpdir` comes
  from `node:os`, or with `t.TempDir()` in Go. Both read `TMPDIR` when it is set, so a Legion pod
  still writes under `/legion/scratch/tmp`.
- Never write `process.env.TMPDIR ?? "/legion/scratch/tmp"` or any other fallback to a path that
  only a Legion pod has. A GitHub runner sets no `TMPDIR` and has no `/legion`. `mkdtemp` then
  throws `ENOENT` while the module loads, before any test runs, and bun reports the file as
  `1 fail`, with no test name.
- Before you call such a test done, run it once with `env -u TMPDIR`, as a runner does.

## Evidence

`scripts/dispatch/__tests__/comment-delivery.test.ts` passed in the pod and turned the required
`test` check red on two heads of sjawhar/legion#2366 (jobs 122114381270 and 122162950817):
`ENOENT: no such file or directory, mkdtemp '/legion/scratch/tmp/comment-delivery-…'`. The daemon
sets `TMPDIR` in every role container, so the fallback never ran in the pod. Commit e1b1ab46 switched to `tmpdir()`, and `test` passed at that
head.
