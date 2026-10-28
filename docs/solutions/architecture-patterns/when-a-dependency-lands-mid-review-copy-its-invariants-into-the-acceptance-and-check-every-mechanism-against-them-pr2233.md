---
title: "When a dependency lands mid-review, copy its invariants into the acceptance and check every mechanism against them"
category: architecture-patterns
tags:
  - review-rounds
  - acceptance
  - invariants
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/docs
---

# When a dependency lands mid-review, copy its invariants into the acceptance and check every mechanism against them

Extends docs/solutions/architecture-patterns/a-rule-that-writes-in-a-humans-name-takes-its-authority-from-rows-the-server-wrote-and-holds-the-tree-to-the-servers-own-writer-LEGION-573.md.

- When a change your work now depends on merges during review, read its invariants from its own
  code and comments and write them into your acceptance list.
- Check every proposed mechanism against that list before you build it, the architect's included.
  Name the invariant a mechanism would break, with its file and line, and propose one that keeps it.
- Converge on invariants, not on mechanisms: once the list is written, each later round is judged
  against it.

## Evidence

In sjawhar/legion#2233, cb3a2711 followed a proposed design: index a closed issue's live text at
its latest version. That broke #2210's own rule in `refs/graph.go` ("A block no current version
holds has no row and resolves to nothing") and dated an unsettled edit to a version that did not
hold it. The tester's red test 2f3a9b7f caught it, and 3a58ed47 undid it. After that the round's
criterion listed the invariants (nothing left owed; every block a version holds resolves; history
never names a version that lacks the text, nor a deletion at a version that holds the block; after
the reopen each block is dated to the first version that holds it; open issues unchanged), and the
later rounds were judged against that list.
