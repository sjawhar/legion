---
title: "A read that hands back the writer's own tree names its callers, states the rule, and re-derives it in a test hook"
category: envoy
tags:
  - aliasing
  - reuse
  - dispatch-docs
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2497"
---

# A read that hands back the writer's own tree names its callers, states the rule, and re-derives it in a test hook

Extends docs/solutions/testing/prove-a-reuse-change-against-an-independent-recomputation-on-the-live-commit-stream-and-assert-every-path-counter-pr2453.md.

When a read starts to return an object the writer still uses (here `docTree` returning the write's
own `liveWrite.tree` instead of a fresh `treeOf(fork)`), a caller that edits it now corrupts the
writer's next operation and version. Go cannot stop that, so:

- Read every caller and confirm each only reads. Name them in the read's doc comment, and state the
  rule where the package's readers look (`packages/envoy/AGENTS.md`, the reads paragraph).
- Put the "is it still valid" check in one predicate (`liveWrite.rendering`: the write holds a
  rendering and the fork is the write's own), and route every reuse site through it. Here the
  version capture, the margin write and `docTree` had grown separate copies of the check.
- Leave reads whose callers are outside the writer's transaction returning copies (`heldTree`
  still reads the fork; its readers own their tree).
- Add a test hook at each reuse site and at publish (`checkRendering(at, write)`) that compares the
  object with a fresh read, both by deep equality and by its serialized form, so a caller that
  edits it fails the next comparison.
- Write the unenforced contract into the PR body's risk line.

## Evidence

In sjawhar/legion#2497, a temporary edit that made `SuggestionKind` set a block id on the tree
from `docTree` failed `TestForkReadsEveryReuseOfAWritesRenderingDescribesItsFork` at the next
`checkRendering` call (`a write's rendering is not its fork's at fork`). The reuse made
`SuggestionKind` inside a suggestion comment 83% faster on real specs.
