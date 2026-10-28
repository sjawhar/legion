---
title: "Match across snapshots by durable identity first, and by text only where the text names one item"
category: architecture-patterns
tags:
  - identity
  - matching
  - block-index
  - history
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/docs
---

# Match across snapshots by durable identity first, and by text only where the text names one item

When you pair the items of two snapshots and only some items carry a durable id:

- Let the durable records (index rows, stored ids) claim their items before any content is compared.
- Match by content only where it is unambiguous: a text held once on each side, or equal copy counts
  paired in order. Otherwise leave the item unmatched.
- Apply the consumer's own rule for a repeated id at the input (here, "the first holder of an id is
  the node", `refs/blocks.go`), so a copy that kept its source's id takes part in nothing.
- Assign every id through one function that refuses an item that has one and an id already taken.
- Leaving an item unmatched only dates it late. A false match rewrites history, for example by
  recording a block deleted at a version that holds it. Prefer the late date.

The version's markdown keeps the ids of typed blocks only (docs/solutions/testing/a-stored-versions-read-back-repairs-a-repeated-block-id-before-any-reader-sees-it-so-a-plans-red-on-base-claim-is-a-hypothesis-until-the-base-run-fails-LEGION-752.md
records that its parse mints the others), so an untyped block's identity across versions exists only
in the index rows.

## Evidence

`packages/envoy/internal/dispatch/docs/closed_version_blocks.go` in sjawhar/legion#2233. Each rule
came from its own round, and each round had a test that failed before it:

- e02af334, rows claim first. The tester's 44cf9cac had read "dated 0 … deleted at version 1 true".
- 49a1d601, repeated text paired only on equal counts. c12da9a2 had read "dated 1 … while closed".
- fd3bfa69, only the first live holder of an id takes part, and `versionMatch.take` is the only writer
  of an id. Before it the matcher gave the ids `[p1 p1]` for a pasted copy.
