---
title: "A block that nests is a micromark container construct, capped at one depth that Go refuses past"
category: dispatch-web
tags:
  - micromark
  - proof-editor
  - untrusted-input
  - main-thread
  - pmdoc
date: 2026-10-28
status: active
module: packages/proof-editor
---

# A block that nests is a micromark container construct, capped at one depth that Go refuses past

Extends docs/solutions/dispatch-web/draw-untrusted-content-with-a-layout-engine-in-a-worker-whose-clock-starts-at-ready-pr2311.md.

- In the browser engine, write a block that can nest as a micromark *document* construct (with
  `continuation` and `exit`, held on the container stack as a quote or a list item is). A flow
  construct whose content is a sub-document tokenizes that content once for each enclosing block,
  so the cost grows with depth times length.
- A container construct still costs O(D²) per line D blocks deep, because micromark copies its
  token stack in every attempt. Cap the depth: an opener past the cap does not open and reads as
  text.
- Hold the cap at one number in three places: the read-only engine reads the opener as text, the
  default editor refuses it naming its line, and Go refuses the write. Each constant's comment
  names the others. Pin the cap with a corpus fixture at N and a `corpus-errors` fixture at N + 1.
- A depth cap alone does not fix a sub-document construct, and a container construct alone does
  not bound the cost. You need both.

## Evidence

- `packages/proof-editor/src/directive-container.ts` replaced micromark-extension-directive's flow
  construct (header comment, lines 5-22). It is capped at `maxDirectiveNesting = 32` (line 116).
  The default editor refuses the 33rd opener in `block-schema.ts`, and Go refuses it at
  `maxTypedNesting = 32` (`pmdoc/nesting.go:25`). The fixtures are `typed-blocks-nested-32-deep.md`
  and `corpus-errors/typed-blocks-nested-33-deep.md`.
- Before the change, 2,000 nested unclosed `:::a` lines then a 21,000-character line took 54.6 s
  in the read-only engine, and 1,000 nested closed blocks 13.9 s. With the container construct and
  the cap: 0.3 s and 0.2 s.
- Measured while planning: the cap alone left 7.8 s at depth 25 for a 32,000-character body, and
  the container construct alone left the first body at 36.7 s.
