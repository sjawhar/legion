---
title: Proof document schema (pmdoc)
parent: dispatch-server
paths: [packages/envoy/internal/dispatch/pmdoc]
---
Parse, render and search over the Proof document tree: the typed-block schema (`schema/blocks.json`), block-id minting and repair, and the fixtures generated from the fork's headless engine (`gen`). A leaf: it imports no other package of the server.

A table row is split into cells as the browser editor splits it: a `|` after an even run of backslashes, none included, ends the cell, and one after an odd run is the cell's text, so `| a \\| b |` is the cells `a \` and `b`, and `| a \| b |` is the one cell `a | b`. Goldmark ends a cell only at a `|` with no backslash right before it, so `table.go` splits its cells where the two differ and counts every row's cells by the browser's rule.
