---
title: "When a forward merge brings main's own copy of a behaviour the branch built, keep one and check the spec against it"
category: legion
tags:
  - forward-merge
  - semantic-conflict
  - acceptance
date: 2026-10-30
status: active
module: skills/legion-worker
---

# When a forward merge brings main's own copy of a behaviour the branch built, keep one and check the spec against it

- Before you resolve a forward merge, read main's side of each conflicted file as a whole change
  (`jj diff --from <fork point> --to main@origin <file>`), not hunk by hunk, and ask whether main
  now does what your branch does. Two copies of one behaviour can conflict in a few lines, or in
  none, and both still compile and pass their own tests. The defect shows only as the behaviour
  happening twice: a thread printed twice, a message delivered twice.
- When main already has it, keep main's copy and remove yours whole: the code, its tests, its help
  or tool-description sentence, its `AGENTS.md` and README text, and its changelog fragments, which
  would otherwise announce the change a second time in your package.
- Then read each acceptance line of your spec against main's version and run its command on the
  merged tree. A line main's version does not meet is still owed by your issue, so close the gap on
  your branch.
- Tell the architect, in the round's report, that the spec point is now delivered by main. Its
  acceptance and the production check then read main's code.

## Evidence

sjawhar/legion#2376 built two behaviours of its spec: `dispatch doc-read` printing each open
anchored comment in full, and a person's reply in a thread on a document reaching the sessions that
wrote the document. Main's #2366 shipped both before the branch merged. At merge `ae693c92`, the
conflicts in `packages/envoy-client/src/dispatch-execute.ts`, `dispatch-http.ts`, the outbox's
`publisher.go` and `comment_create.go` were each a few hunks. Taking both sides would have printed
every thread twice and routed replies by two rules. The merge kept main's code, then removed the
branch's printing, its `suppressed_authors` routing, their tests, the doc-read help sentence and its
test, two fragments, and the routing paragraph in `packages/envoy/AGENTS.md`. Checked against the
spec afterwards, main's help text did not name the root comment's time, which the spec asks for.
The reviewer fixed that on the branch in `855d6679`.
