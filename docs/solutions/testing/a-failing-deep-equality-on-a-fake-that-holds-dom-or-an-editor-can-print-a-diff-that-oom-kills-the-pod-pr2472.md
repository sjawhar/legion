---
title: "A failing deep-equality on a fake that holds DOM or an editor can print a diff that OOM-kills the pod"
category: testing
tags:
  - bun-test
  - out-of-memory
  - issue-pod
  - happy-dom
  - assertions
date: 2026-10-29
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2472"
---

# A failing deep-equality on a fake that holds DOM or an editor can print a diff that OOM-kills the pod

- Assert counts and named scalars on a recorded fake (`toHaveLength`, `.destroyed`,
  `.focused`), never `toMatchObject`/`toEqual` on an array of fakes that reference DOM nodes, a
  ProseMirror view or an editor handle. When such an assertion fails, the runner renders a diff of
  the whole object graph before it reports anything.
- When a pod's containers die together with no failing line in any log, suspect a test that is
  failing, not one that hangs. Run the suspect files one at a time under an RSS cap on the
  process group (poll `ps -o rss= -g <pgid>` and kill the group past the cap), so the run stops
  with a number instead of taking the pod down.
- Treat that failure as a real defect first: the diff was large because the assertion failed.

## Evidence

In sjawhar/legion#2472, a change made `VersionView` focus a past version's highlight twice.
`VersionView.test.tsx` asserts `editors[0]` with `toMatchObject` on the fake runtime's editor
(which holds its happy-dom root), so it failed and bun's diff grew past 3 GB in 20 s. The whole
issue pod (six containers) was OOM-killed several times, first blamed on browser runs. A capped
runner killed the run at 3,018 MB on `VersionView.test.tsx` alone and showed the cause; with the
double focus fixed (53fe210f) the full Dispatch unit suite peaked at 1.2 GB. The round-4 lock in
the same file asserts only lengths and named fields.
