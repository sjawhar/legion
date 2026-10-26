---
title: Proof document schema (pmdoc)
parent: dispatch-server
paths: [packages/envoy/internal/dispatch/pmdoc]
---
Parse, render and search over the Proof document tree: the typed-block schema (`schema/blocks.json`), block-id minting and repair, and the fixtures generated from the fork's headless engine (`gen`). A leaf: it imports no other package of the server.

A table row is split into cells as the browser editor splits it: a `|` after an even run of backslashes, none included, ends the cell, and one after an odd run is the cell's text, so `| a \\| b |` is the cells `a \` and `b`, and `| a \| b |` is the one cell `a | b`. Goldmark ends a cell only at a `|` with no backslash right before it, so `table.go` splits its cells where the two differ and counts every row's cells by the browser's rule.

So code in a cell cannot hold a `|` after an odd run of backslashes (`\|`): in a cell a `|` is written `\|`, in code too, `\\|` ends the cell, and a backslash escapes nothing in code. A caller's write that leaves such code in a cell (a replace, a delete by quote, an accepted suggestion) is refused with `TableCodePipeError`, which names the table's block id, the cell's row and column and the header above it, and says to drop the backslash or end the code before the `|`; a table that already held such code is no reason to refuse a write into it. A browser edit holding it is versioned at settlement as its rendering, as every browser edit is: `render_table.go` (`codePipesOutOfCode`) writes each such `|` as text after the code, so `a\|b` in code is stored `` `a\`\|`b` ``, which both parsers read back as the same cell with every character, that `|` no longer code.
