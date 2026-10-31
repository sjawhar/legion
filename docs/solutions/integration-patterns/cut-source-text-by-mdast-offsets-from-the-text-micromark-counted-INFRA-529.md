---
title: "Cut source text by mdast offsets from the text micromark counted"
category: integration-patterns
tags:
  - micromark
  - mdast
  - remark
  - offsets
  - line-endings
  - byte-order-mark
date: 2026-10-27
status: active
module: packages/proof-editor
related_issues:
  - "INFRA-529"
  - "sjawhar/legion#2303"
---

# Cut source text by mdast offsets from the text micromark counted

When code slices `file.toString()` with mdast `position.offset` values, it must read the text as
micromark reads it:

- **Remove one leading U+FEFF before you index.** micromark drops a leading byte-order mark before
  it counts offsets. Without this, every offset is one character early, and the cut text loses or
  shifts its first and last characters.
- **End a line at `\r`, `\n` or `\r\n`.** A scan to the next `\n` alone runs past a lone CR. On a
  body with lone-CR line endings it then repeats text, and the output grows with the square of
  the input length.
- **Decide what a cut line is by its syntax, not by its last characters.** A directive's closing
  fence is a line that holds only container prefixes (`>`, list indentation) and a colon run at
  least as long as the opening run. It must also start after the directive's last kept child. A
  line that ends in `mailto:` or `a:::` is not a fence.
- **Test with stored bodies, not only with document text.** The Dispatch server converts line
  endings where text enters a document, and for ask questions. It stores comment and message
  bodies as sent, so a client that reads them must have test rows for CRLF, a lone CR and a
  leading BOM.
- **Name each other reader of the same offsets in the review.** Two readers that cut differently
  disagree on the same body.

## Evidence

- `packages/proof-editor/src/typed-block-drawing.ts:42-45` is `markdownSource`, the one helper that
  returns the source with one leading U+FEFF removed (the U+FEFF fix landed in `6af96b96`, in
  `read-only-directives.ts`; INFRA-565 moved it and the code below into `typed-block-drawing.ts`).
  Before that fix, `"\ufeff:::note\nbody\n:::\n"` lost its closing `:::`, and
  `"\ufeffHello **b**\n\n:::note…"` drew an empty opening line (`RangeError: Empty text nodes are
  not allowed`).
- `:125-135` in `drawDirective` ends lines at CR or LF (fixed in `75cd715e`). Before that,
  `":::a\r:::\r".repeat(3555)` (31,995 characters) drew 44,245,530 characters; after, 24,885.
- `:36` and `:138-152` hold the closing-fence rule (fixed in `75cd715e`).
- Browser rows 3 and 4 of `packages/dispatch/e2e/unknown-directive.e2e.ts` post a lone-CR message
  and a BOM-led message through the real API and assert each line once.
- Both callers now read the source through `markdownSource`: read-only mode's transform and default
  mode's `remarkTypedBlocks` (`packages/proof-editor/src/block-schema.ts:287`), which INFRA-565 moved
  off `file.toString()`. Before that move, a leading U+FEFF shifted default mode's paragraph-line
  refusal by one character: `"\ufeffhello\n::a"` parsed, and `"\ufeffx\n\n::: warning"` was refused
  as `line 4:`. `packages/proof-editor/tests/typed-block-drawing.test.ts` pins both.
- Related: `docs/solutions/testing/one-definition-implemented-in-go-and-in-the-browser-is-pinned-by-fixtures-both-suites-read-one-per-shape-the-two-parsers-could-disagree-on-LEGION-573.md`.
