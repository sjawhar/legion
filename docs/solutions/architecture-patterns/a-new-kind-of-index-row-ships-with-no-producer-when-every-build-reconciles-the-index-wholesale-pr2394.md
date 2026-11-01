---
title: "A new kind of index row ships with no producer when every build reconciles the index wholesale"
category: architecture-patterns
tags:
  - dispatch
  - rolling-deploy
  - migration
  - block-index
  - expand-contract
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch
---

# A new kind of index row ships with no producer when every build reconciles the index wholesale

Extends docs/solutions/architecture-patterns/a-graph-read-skips-a-node-kind-its-build-cannot-address-and-one-registry-drives-lists-and-counts-INFRA-364.md,
which covers the read direction. This document covers the write direction.

## The rule

- When each re-index of a document replaces all of that document's rows in an index, a build that
  does not know a new kind of row deletes that kind on its next re-index. In a rolling deploy, the
  previous build is still serving.
- So the first change ships the schema, the reader and the writer, and declares nothing that
  produces a row of the new kind. A later change declares the first producer, once every serving
  build knows the kind. That change then rebuilds the index after its roll completes.
- Keep that first migration expand-only: a nullable column the previous build neither reads nor
  writes (its rows keep null, which means "no limit"), a function, and a restated view.
- Write down, in the pull request's risk section, why nothing can be lost in that window.

## Evidence

- Migration `0148_block_pointer_links` adds `block_refs.to_types` with no default, the function
  `block_ref_listed` and the restated `graph_edges` view. Its header says why the previous build
  serves unchanged.
- No shipped type in `pmdoc/schema/blocks.json` declares a `block[]` attribute. With none declared,
  `pmdoc.HasLinkKinds` is false, and `docs.blockPointers` indexes no pointers
  (`docs/reference_excerpts.go`).
- sjawhar/legion#2394's body, under "Not proven / risk": "a rolling deploy where any serving build
  cannot read a new typed type, or declares no `block[]` attribute, deletes pointer rows on its
  next index of a citing document; this PR ships no such type, so no row exists to lose."
