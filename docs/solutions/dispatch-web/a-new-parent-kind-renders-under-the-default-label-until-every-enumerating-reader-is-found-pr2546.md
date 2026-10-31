---
title: "A new parent kind renders under the default label until every enumerating reader is found"
category: dispatch-web
tags:
  - reference-graph
  - backlinks
  - discriminated-union
  - contract-census
  - browser-proof
date: 2026-10-31
status: active
module: packages/dispatch/web/src/features/refs
related_issues:
  - "sjawhar/legion#2546"
---

# A new parent kind renders under the default label until every enumerating reader is found

Extends docs/solutions/legion/a-type-census-also-searches-the-types-that-contain-it-their-re-exports-and-indexed-access-types-INFRA-367.md.

That note's census misses cost nothing because nothing read the widened field. Where a consumer
*enumerates* the members of such a union, a miss costs a wrong label on a page a person reads.

- A kind arrives from the server as a wire string, so a reader that lists the members it handles
  and falls through to a default **degrades instead of failing a typecheck**. Nothing in the
  build tells you the list is short; the only signal is the rendered page.
- Grep every consumer that enumerates the old members, by the member names rather than by the
  type, and include the files your change never opens — a client that prints the same edge as
  text has the same list, written separately.
- Replace each enumeration with the one exported set, so the member list is a single import and
  the next kind is one line.
- **Prove the fix on the real page**, with a row in the browser suite that asserts both the label
  the source must now carry and the absence of the default one. A unit test over the component
  passes with either list when its fixture names only the old kinds.

## Evidence

sjawhar/legion#2546 made an ask a third parent of a block node.
`packages/dispatch/web/src/features/refs/ReferencedBy.tsx`'s `blockItem` accepted only
`comment` and `message` parents, so a block node with the new parent was not recognised as an
item and fell through to `NODE_LABELS.block`, which is `"Artifact"` (`:20-26`, `:60`): the
backlink panel read `Artifact · KEY` where the base read the item's own label.
`packages/envoy-client/src/dispatch-execute.ts`'s `edgeLine` held the same gap in its own
printed form.

`dd2f17197c3f` fixed both and moved the member list into one place:
`BLOCK_ITEM_KINDS`/`isBlockItemKind`
(`packages/dispatch/web/src/features/refs/block-reference.ts:20-27`), which `blockItem`
(`ReferencedBy.tsx:53`) now calls instead of comparing two literals.

The proof is the browser row in `packages/dispatch/e2e/ask-part-references.e2e.ts:127-142`: it
opens the issue header's "Referenced by" region, requires the link named `Ask · <key>`, and
asserts `references.getByText(/^Artifact · /)` has count 0 — the negative that fails if the
enumeration is short again.
