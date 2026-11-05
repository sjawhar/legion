---
title: "A node loader feeds every graph reader, so it holds the strictest visibility rule and no whole text"
category: architecture-patterns
tags:
  - reference-graph
  - authorization
  - node-loader
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/refs
---

# A node loader feeds every graph reader, so it holds the strictest visibility rule and no whole text

Extends docs/solutions/architecture-patterns/a-graph-read-skips-a-node-kind-its-build-cannot-address-and-one-registry-drives-lists-and-counts-INFRA-364.md.

One registry entry makes a kind resolve, list and count everywhere; it also exposes the kind everywhere.

- When a new node kind sits under a parent whose reads are restricted, put the parent's visibility rule in the loader's own query. The references route resolves a node by its id whatever owner key the reference spells, so a route-level read check elsewhere does not cover it.
- A loader gives a node its facts and the edge its excerpt; only the handler of a single-node read adds whole text (as docs/solutions/architecture-patterns/read-display-only-data-in-the-handler-that-shows-it-not-in-a-query-helper-other-callers-share-INFRA-569.md says for display-only data).
- Test a plan's "reachable only through route X" by reading the node through every other route, with an unrelated owner key and with one that does not exist.

## Evidence

sjawhar/legion#2463 planned that a direct message's blocks are reachable only through `GET /api/v1/messages/{id}/blocks`, under the conversation's read rule. The tester read `GET /api/v1/references?to=dispatch://ANY-1/message/<dm>#b-b1` with any key, even one that does not exist, and got 200 with the message's text. `loadMessageBlockNodes` (`refs/item_block_nodes.go`) now reads only an issue message's blocks, and `TestADirectMessagesBlockHasNoAddress` loops over two keys. Review then found every edge-list node carrying its block's whole text, up to the body cap per edge; the loaders now set none, and only `itemBlockReferenceNode` (`api/references.go`) reads it through `refs.CurrentItemBlockText`.
