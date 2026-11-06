---
title: "A corpus writes each construct alone in its block, and a differential gates what it cannot compare"
category: envoy
tags:
  - pmdoc
  - differential
  - corpus
  - fixtures
  - parser-parity
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2518"
---

# A corpus writes each construct alone in its block, and a differential gates what it cannot compare

- **Generate each construct in three positions, not one:** with text around it, alone as its
  block's whole content, and in each mark context, every one of those crossed with every container
  (paragraph, heading, quote, bullet and ordered item, table cell, callout, footnote definition,
  quoted item). A row that always has words beside it never reaches the block parser, so it can
  never show a block-level divergence. Say in the generator's header which products it writes and
  which it leaves out, with the reason — a form holding a line ending belongs only in the
  containers whose body runs at column 0.
- **A committed corpus fixture is held to a stricter rule than a generated corpus, so the two
  differ on purpose.** Every fixture document must round-trip (`TestRenderParseRoundTrip`), and a
  block whose whole content is one inline leaf of raw HTML renders to a line both readers read as
  an HTML block, which the Proof schema has no node for. Keep each fixture row inside a text run
  (`a **<b>** b`, `p **<!a>** q`) and leave the bare form to the generated corpus, where a refusal
  on both sides is a comparison and not a failure.
- **Every class of document your A/B harness cannot compare gets its own counted, failing gate.**
  A document one reader produces and the other's schema refuses is exactly the class a parity
  change is about; counted beside an engine throw, with no gate reading it, such a document drops
  out of every gate and the run still passes.
- **A gate above zero merges only as a named class, accepted before the merge.** Name the class,
  show that every document under it is that class, say in one sentence why zeroing it would be
  wrong, and re-measure it in the next round: a class is what survives re-measurement. Report each
  class separately, never as one total.

## Evidence

sjawhar/legion#2518 ran `gen/differential.sh` over 40,142 documents at every round.

- Its new `gen/leaf-marks-corpus.ts` writes content form × mark context × container, 2,005
  documents. The committed `testdata/corpus/html-declarations.md` holds the same constructs with
  words around them, and that difference is where the change's three non-obvious counts came from:
  44 documents of a lone `<!a>` line, and 111 of `**<!a>**`, `*<!a>*` and `[<!a>](u)` in the nine
  containers, where the mark leaves the html node alone in the block. The fixture file alone would
  have shown none of them.
- Two corpus files had to be rewritten during the change because their rows stood alone in their
  blocks: the rendering of `**<b>**` is the line `<b>`, which `Parse` refuses as block HTML, so
  `TestRenderParseRoundTrip` failed on the fixture rather than on the change.
- The differential's canonicalisation of the engine's reading was deleted when the engine stopped
  marking inline leaves, and that left a reading Go's schema refuses counted under `engine cannot
  read`, which no gate read. `differential_test.go` now tells an engine throw from a schema refusal
  and gates the second (`engine readings Go's schema refuses`), 0 at the merged head.
- Two gates stayed above zero and merged as named classes, each accepted before the merge and
  unchanged across three further rounds: 181 `engine-tree churn`, every one an image label holding
  raw HTML, where the base had to escape the alt and the head writes the author's bytes, both
  readings reading back; and 111 `newly refused by the writer` with 44 `newly refused`, every one a
  block whose whole inline content is a declaration, which the head refuses by name as it already
  refused a lone `<b>`.
