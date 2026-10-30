---
title: "Never jj split a merge commit: write the next file into a jj new child"
category: legion
tags:
  - jj
  - forward-merge
  - handoff
date: 2026-10-30
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2529"
---

# Never jj split a merge commit: write the next file into a jj new child

Extends docs/solutions/legion/long-lived-branch-mechanics-jj-new-and-merge-not-rebase.md.

After a forward merge the working copy sits **on the merge commit**. Every file you add next —
the round's handoff above all — goes into a `jj new` child:

```bash
jj -R "$LEGION_WORKSPACE" new -m "<message>"   # then write the file
```

**Never `jj split` there.** `jj split` puts the selected paths in the *first* commit, which keeps
the original parents, and the remainder in a new child. On a merge that inverts the two: the
handoff lands inside the merge, the resolution is exiled to the child, and the merge — whose
content is its parents' auto-merge plus its own diff — is left holding the materialised conflict.
`jj undo` recovers it, and in a workspace that is its own clone (the Kubernetes runtime's issue
pod) that undo touches nothing else; where the operation log is shared, recover forward instead.

The collision is in the instructions, not in jj: the worker skill gives `jj split` as *the* way
to commit a phase's work, and the forward-merge procedure leaves `@` on a merge. They meet exactly
once per forward merge.

## Evidence

- jj in the worker image is `0.45.1-sami.20260910-043938`, pinned by `JJ_TOOL` in
  `packages/daemon/docker/worker.Dockerfile`.
- sjawhar/legion#2529: `jj split -m "implement: record handoff" .legion/<issue>/implement.json`
  run on the forward-merge commit produced a first commit described `implement: record handoff`
  carrying the merge's two parents and reported in conflict, with the resolution moved into a
  child described `merge: resolve conflict against main@origin`. One `jj undo` restored the
  pre-split state; the handoff was then written into a `jj new` child.
- The shape the pull request ended with is the right one: merge `54644c3d1fa0` carries the
  resolution and the behaviour change it forced, and `2b1c0b2313cc` is a child adding
  `.legion/<issue>/implement.json` alone.
