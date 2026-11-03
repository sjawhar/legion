---
title: "A whole and each of its parts are hashed from one encoding by byte span, and each keeps its own error scope"
category: envoy
tags:
  - tokens
  - hashing
  - one-pass
  - error-parity
  - performance
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2451"
---

# A whole and each of its parts are hashed from one encoding by byte span, and each keeps its own error scope

Extends docs/solutions/envoy/a-per-request-decode-memo-keys-on-the-stored-bytes-clips-what-it-shares-and-leans-on-insert-only-columns-pr2425.md.
That document says a shared decode returns "the same error text as before, so the first bad row
still fails first". This one adds that one pass over a whole and its parts must also keep each
error scope apart, not only the first error.

- When a tree and each of its nodes need a canonical hash, encode the tree once and record each
  node's byte span as it opens and closes. Then hash the whole buffer and each sub-slice. Do not
  encode each node again: a nested node is then encoded once per ancestor that also needs a hash,
  so the work grows with the tree's size times its depth. This works only when a node's encoding
  does not depend on where the node stands (no indentation, no prefix, no separator inside the
  node's own bytes).
- In Dispatch, a new caller that needs block tokens takes them from `encodedTokens` or
  `documentTokens` (`docs/edits.go`). It never calls `nodeToken` once per block.
- One pass merges error scopes that were separate. A value outside every part fails the whole but
  no part, and a value in one part fails that part and the whole. Return each scope's result and
  error side by side. When the one pass fails, fall back to the old per-part encoding only to learn
  which parts still have a result and which error comes first. Only a tree that fails pays for the
  fallback. Keep each error's wrapping and its order as each caller returned it before.
- A caller that needs only the whole's error (an edit precondition naming blocks, which fails when
  the document does) calls the one pass directly and skips the fallback.
- When one id is on several nodes, insert the spans into the map in pre-order, so the old winner
  (the last in pre-order) still wins.

## Evidence

- `pmdoc.AppendTokenJSON` (`pmdoc/token.go:41`) records a `TokenSpan` per block-id node.
  `docs/edits.go:198` `encodedTokens` hashes the buffer and each span with `tokenHash`.
- `docs/edits.go:214` `treeTokens` holds `documentErr` beside `blocksErr`. `:225` `documentTokens`
  falls back to `nodeToken` per block through `pmdoc.Walk` only on error. `:291`
  `checkEditPrecondition` calls `encodedTokens` alone.
- `docs/mutation.go:633` `renderedBlocks` wraps `blocksErr` in `documentSchemaError`, as before.
  `docs/read_cache.go:255` `buildDocumentRead` returns the document's error unwrapped, after the
  rendering's.
- `docs/tokens_test.go:182` `TestDocumentTokensKeepEveryBlockWhenOnlyTheDocumentCannotBeEncoded`
  puts a NaN in the document node's own attributes: every block keeps its token, and only the
  document fails.
- On the Dispatch load test in `packages/envoy/loadtest` (sjawhar/legion#2369), Dispatch's CPU
  fell from 66.8-69.4 to 45.9-48.1 CPU-s per 300 s, and the fold's share of the profile fell from
  40% to 20% (PR body, `E2E (implementer)`).
