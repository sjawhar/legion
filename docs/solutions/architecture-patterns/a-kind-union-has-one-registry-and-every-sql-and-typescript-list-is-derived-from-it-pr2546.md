---
title: "A kind union has one registry, and every SQL and TypeScript list is derived from it"
category: architecture-patterns
tags:
  - reference-graph
  - registry
  - duplication
  - simplify-pass
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/refs
related_issues:
  - "sjawhar/legion#2546"
---

# A kind union has one registry, and every SQL and TypeScript list is derived from it

Extends docs/solutions/architecture-patterns/a-node-loader-feeds-every-graph-reader-so-it-holds-the-strictest-visibility-and-no-whole-text-pr2463.md.

Those notes hold the loader registry to one map entry per kind. Adding the next kind shows where
the registry is *not* yet the source: every hand-written list of the same members, in SQL and in
the clients.

- Derive each SQL list of kinds from the registry with a small function that quotes and joins it,
  and give the derived values names (one for the item kinds, one for the kinds with a document's
  block kind beside them). A predicate written from a literal tuple admits the kinds that existed
  when it was written.
- Export the union once per client package and import it; a `const` list plus a
  `(typeof LIST)[number]` type covers both the runtime check and the type. A second package with
  its own surfaces (a dashboard's labels and routes) may hold its own named set, but it holds one.
- **After adding a kind, grep the *sibling* kind's own names in the package and diff the two
  arms.** The duplicate is rarely in the file you edited: it is the sibling's arm in another
  file, reached from a different caller, which a local simplify pass never sees. Grep the
  sibling's table name and its enum member, and read the two arms side by side.
- A rule that belongs to the indexing package stays there. A read package that re-derives how an
  index identifies its rows will drift from it the first time the identity changes; call the
  indexing package's own function instead.

## Evidence

sjawhar/legion#2546 added a third item kind to the graph.

- `packages/envoy/internal/dispatch/refs/refs.go` and `refs/graph.go` each wrote the block kinds
  out by hand (`not in ('comment_block', 'message_block', 'ask_block')`) although `itemKinds`
  already existed. `dd2f17197c3f` added `blockKindList` with `itemBlockKinds` and
  `blockNodeKinds` (`refs/graph.go:144-165`), and every such predicate now reads from them.
- TypeScript held four copies of the one union. Three in `packages/envoy-client`
  (`dispatch-execute.ts`'s own `const ITEM_KINDS`, and `dispatch-owner.ts`'s `dispatchChildRef`
  parameter and `item.kind` field) collapsed in `1b769383daf4` into one exported
  `ITEM_KINDS`/`ItemKind` (`packages/envoy-client/src/dispatch-owner.ts:57-60`), which
  `dispatch-execute.ts` imports. The dashboard keeps its own, named once:
  `BLOCK_ITEM_KINDS`/`isBlockItemKind`
  (`packages/dispatch/web/src/features/refs/block-reference.ts:20-27`).
- The registry that worked as designed is `nodeLoaders` (`refs/graph.go:457`): the new kind was
  one map entry.
- Both duplicates the review found were the sibling kind's arm elsewhere.
  `refs/reply_blocks.go`'s new arm was its comment arm with the table and key column substituted;
  `dd2f17197c3f` replaced the three-case `switch` with one query built from `itemTables` and
  `repliesFrom`. And `api/ask_rows.go` had re-derived the indexing package's own identity rule
  (match a stored block to the value it stands for by type and text hash); `1b769383daf4` deleted
  107 lines of it and called `refs.AskPartIDs` (`refs/ask_blocks.go:343`).
