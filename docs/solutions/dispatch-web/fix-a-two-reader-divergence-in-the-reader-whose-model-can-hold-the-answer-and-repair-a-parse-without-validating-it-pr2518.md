---
title: "Fix a two-reader divergence in the reader whose model can hold the answer, and repair a parse without validating it"
category: dispatch-web
tags:
  - proof-editor
  - milkdown
  - prosemirror
  - mdast
  - parser-parity
date: 2026-10-30
status: active
module: packages/proof-editor
related_issues:
  - "sjawhar/legion#2518"
---

# Fix a two-reader divergence in the reader whose model can hold the answer, and repair a parse without validating it

- **Before choosing which reader moves, ask which of the two can represent the other's reading.**
  The reader whose data model forbids the answer is not the one to move. Where neither can hold the
  other's, move both to the one spelling each already reads alike — the spelling the writer
  already produces — rather than inventing a third reading.
- **Never append a mark of a type the active set already holds.** ProseMirror's `Mark.addToSet`
  excludes a mark of a mark's own type, so a tree carrying two of them exists in no editor; its
  rendering does not read back, and the write path refuses a document a person can type into a
  comment.
- **In the engine, a divergence about tree shape is fixed in a remark transform; one about marks
  in a post-parse ProseMirror walk.** Put the transform after every plugin that rewrites what it
  reads, and apply the walk at *every* parse seam — the headless parser, the editor's shared
  `parserCtx` and `setMarkdown` each build their own — beside any walk that may return its input
  unchanged, never inside it. A tree a walk changed is discarded by one that returns its input when
  nothing was missing.
- **A repair walk never validates.** Build it with `node.mark` and `node.copy`, never through
  `Node.fromJSON` or `doc.check()`: the preset's own parsers emit values its schema declares
  invalid, and on a display surface a throw is drawn as the body's literal source.

## Evidence

sjawhar/legion#2518 closed three divergences between `packages/proof-editor` and Go's reader
(`packages/envoy/internal/dispatch/pmdoc`), and each decided the same way.

- The engine kept an emphasis, strong, strikethrough or link mark on an inline leaf (`html`,
  `image`, `hardbreak`, `footnote_reference`). Go cannot hold one — `pmdoc/node.go` refuses a mark
  on any node that is not text — and y-prosemirror drops it on the next collaborative round trip,
  so the engine moved: `src/editor/schema/leaf-marks.ts`, stated structurally
  (`isInline && !isText && isLeaf`) rather than as a list of four names, composed beside
  `withBlockIds` at all three seams, with one test per seam so a failure names the seam.
- An inline HTML declaration is an `html` node in both models and goldmark is the outlier, so Go
  moved there instead.
- An autolink inside a link label (`[a <https://u.test> b](u)`): Go stacked the label's link under
  the autolink's, which no ProseMirror tree holds, and `@milkdown/transformer` keeps one flat mark
  set whose `closeMark` removes by type (`lib/index.js:116-122`, with the link runner at
  `@milkdown/preset-commonmark/lib/index.js:350-360`), so the engine lost the label's link from
  every run after the inner autolink. Both moved to `[a ](u)<https://u.test>[ b](u)`, the spelling
  Go's renderer already writes and both readers already read alike: Go by dropping the active link
  before appending the autolink's, the engine by `src/nested-links.ts`, an mdast transform that
  hoists a link out of a link label, since the disagreement is pure mdast shape.
- The repair walk's rule has a measured reason: the commonmark preset's image parser passes mdast's
  `title` through as `null` where its own schema declares `title: { default: '', validate: 'string' }`,
  so a validating walk throws `Expected value of type string for attribute title on type image, got
  null` for thousands of ordinary documents.
