---
title: "An inline reader moves with its block parser, wrapped in the library's own slot, and remembers per block where no closer remains"
category: envoy
tags:
  - pmdoc
  - goldmark
  - parser-parity
  - raw-html
  - performance
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2518"
---

# An inline reader moves with its block parser, wrapped in the library's own slot, and remembers per block where no closer remains

- **Change what an inline parser opens at a trigger, and change the block parser that opens at the
  same trigger in the same pull request.** Give the trigger one definition both ask, and name every
  caller of it in that definition's comment. A construct that can be a whole block is exactly where
  two readers go on disagreeing: the inline half alone stores a document the other reader reads as
  a block.
- **To change a vendored parser, wrap it in its own slot rather than adding one beside it.** Find
  it by type in the library's own list, keep the priority the library gave it, delegate every case
  you do not change, and let your writer ask the same object, so the two cannot drift. A parser
  registered at another priority leaves the library's walk to run after yours declines, which
  doubles the work and leaves the library's own defects in place.
- **A parser that gives up by scanning to the end of its block needs a per-block, per-kind record
  of the offset from which no closer remains.** Refuse every later opener of that kind at or after
  that offset without scanning again, keyed by the block, since a closer in another block closes
  nothing in this one. State in the record's doc *why* the parser gives up, and say which
  dependency version that assumption was read in: a memo over a third party's control flow is a
  version-pinned claim, and nothing in CI re-checks it when the dependency is bumped. Add a
  linearity row per kind, and have the doc state the invariant rather than the timings.
- **A kind added to the dispatch later keeps the guarantee only by recording what its own walk
  answered.** Write that sentence beside the kind list, not only in the record.

## Evidence

sjawhar/legion#2518 made Go's reader (`packages/envoy/internal/dispatch/pmdoc`) read an inline HTML
declaration at `<!` plus any ASCII letter, as CommonMark 0.31 and the browser editor's engine do,
where goldmark opens one only after an uppercase letter.

- The inline half alone left 149 documents of a 39,889-document differential in a class of their
  own: a line holding a lone `<!a>`, which Go then stored as a paragraph of inline raw HTML and the
  engine read as an HTML block, a reading the Proof schema has no node for. goldmark's HTML block
  type 4 is `^[ ]{0,3}<![A-Z]+` (`parser/html_block.go:88` at v1.8.6). The block half closed it, and
  `opensDeclaration` (`raw_html.go`) is the one definition its three callers ask.
- The first shape of the inline reader was a parser of its own at priority 350, before goldmark's
  at 400. An unclosed `<!a` was then walked to the block's end twice — once by each parser — and
  again from every later opener: a 1 MiB paragraph took 9.4 s to `Parse` and 18.6 s to
  `ParseForWrite`. Wrapping goldmark's parser in place (`emphasis.go`'s `inlineParsers`, by
  `reflect.TypeOf`, the idiom the package already used for its link parser) and recording per block
  and per kind took both to 0.21 s and 0.55 s, and made goldmark's own long-standing quadratic
  scans of an unclosed `<!--`, `<?` and CDATA linear as well (1 MiB: 24.97 s / 50.17 s → 0.23 s /
  0.55 s for a comment, 46.31 s / 93.65 s → 0.22 s / 0.62 s for a processing instruction).
- The record is sound only because goldmark gives up for one reason, no closer follows
  (`parser/raw_html.go:80-119` at v1.8.6); a fuzz of 60,000 documents with the record on against off
  found no difference, and that fuzz guards v1.8.6 alone.
- `linear_test.go`'s `TestUnclosedRawHTMLShapesParseAndRenderLinearly` holds one row per kind, and
  the type's own doc states the invariant: each block is walked to its end at most once per kind.
