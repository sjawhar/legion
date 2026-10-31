---
title: "Keep a frozen copy of the old walk in the tests and hold a rewrite to it on real and synthetic trees"
category: envoy
tags:
  - pmdoc
  - performance
  - equivalence-check
  - corpus
  - benchmark
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
supersedes:
  - docs/solutions/envoy/index-a-slow-pairing-walk-by-exactly-what-its-predicate-reads-and-prove-the-index-against-the-walk-INFRA-534.md
related_issues:
  - "sjawhar/legion#2455"
---

# Keep a frozen copy of the old walk in the tests and hold a rewrite to it on real and synthetic trees

This supersedes one bullet of the file above, "Remove the switch and the copy of the old walk in
the same change": keep the copy, as a test-only reference, so the next change to the walk is
checked against it too. That file's evidence admits its harness was not committed and must be
rebuilt. Everything else in it stands.

When you rewrite a pmdoc walk or measure for speed and its results must not change:

- Copy the old algorithm into the `_test.go` file as `reference…` functions. Freeze only what the
  change touches (here the walk and `len16`). Call the package's own schema facts the change did
  not touch (`isLeafNodeType`) rather than copying them: a copied table pins the schema, and a
  correct schema change then fails the test.
- Compare every observable of each call: the node pointer, a copy of the path, nil-ness of the
  path, both positions, early stops at several points, and a panic in the same place. An early
  stop's expected visits are a prefix of the full reference run.
- Add an opt-in test over real documents, gated on an environment variable that names a
  directory of markdown (the `PMDOC_DIFF_*` pattern of `differential_test.go`). Fetch the corpus
  read-only under `$TMPDIR` and never commit it. Failures name a node by type, path, range and
  text length, never its text.
- Put each corpus document through the path the server walks: `Update` into a Y fragment, marks
  over part of a text (a whole-text mark splits no run), `Read` back. Walk both trees.
- Log a census of what the corpus held (characters above U+FFFF, empty texts, depth). Cover each
  case it lacked with a synthetic tree in the always-run test.
- To show a cost that grew with depth is gone, benchmark two documents of the same node count that
  differ only in depth, at base and head. Run the head's benchmark file in a base export, with a
  stand-in for any test helper base lacks.

## Evidence

- sjawhar/legion#2455 keeps `referenceWalk`, `referenceNodeSize` and `referenceLen16` in
  `pmdoc/walk_test.go` and `position_test.go`. Review round 1 (thread PRRT_kwDORFy7ds6xKhsp)
  removed a copied leaf list for the reason above.
- `TestWalkMatchesTheReferenceWalkOnACorpus` over 1,200 real specs found 0 mismatches. Its census
  showed no character above U+FFFF and no empty text in any of them, so only the synthetic trees of
  `TestWalkMatchesTheReferenceWalk` cover those. Marks over whole texts left every node count
  unchanged; marks over the middle third split 84,618 runs.
- At 60,000 nodes the base walk took 21.98 ms at depth 5 and 25.91 ms at depth 20; the head took
  5.04 and 4.13 ms.
