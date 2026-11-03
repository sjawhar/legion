---
title: "A rewriter that must not change rendering is safe by what it refuses, and by the renderer readers use"
category: best-practices
tags:
  - markdown
  - codemod
  - formatter
  - cmark-gfm
  - micromark
  - refusal
date: 2026-10-28
status: active
module: scripts
related_issues:
  - "sjawhar/legion#2422"
---
# A rewriter that must not change rendering is safe by what it refuses, and by the renderer readers use

- Name the constructs the rewrite models. Leave every other region byte for byte, or refuse the
  file. Do not chase counterexamples one construct at a time: each fix moves the edge, and the next
  review finds a new case past it.
- Make a region verbatim by a rule that needs no parsing of its end. For example, everything from
  the first opening of a raw-text HTML element to the end of the file. Do not count close tags
  that another parser may read differently.
- Before any write, run checks that do not share the rewriter's model: the text is equal once
  whitespace is folded, a real parser renders the same tree, and a second pass changes nothing.
  Any failure writes nothing and names the file and the block.
- When that parser is not the one readers use (micromark or CommonMark 0.31 in place of GitHub's
  cmark-gfm), list where the two disagree. Refuse each input that touches those places, before
  the rewrite. Prove the tool against the real renderer, such as GitHub's `POST /markdown`.
- Give every refusal test an input that the rewrite would otherwise change. Otherwise a refusal
  that still writes the file passes its test.

## Evidence

- `scripts/semantic-line-breaks.ts` (sjawhar/legion#2422) took about eleven review rounds. Each
  case-by-case fix (inline tags, setext headings, front matter, close-tag nesting) leaked. The
  rounds ended at a09123df (verbatim from the first raw-HTML open to the end of the file, no close
  counted; `markVerbatim`, `exactBlocks`).
- Its three guards run in `writeFiles`: `foldedDifference`, `renderDifference` (micromark with GFM
  and front matter), and `secondRelayoutDifference`.
- Reviewers found places where micromark and cmark-gfm disagree with GitHub's renderer: `<source`
  still starts a kind-6 HTML block there (bbcf3e77); `\v`, `\f` and a bare `\r` are whitespace and
  line endings there (7fd7eee9); CRLF and invalid UTF-8 (4e67dd97); one leading byte order mark is
  dropped, a second one is text (faeb4057, fc24d837). The final defence is the refusal
  `ODD_WHITESPACE = /[\v\f\r\uFEFF]/` and a fatal `TextDecoder`, both before the rewrite.
- Round 9 found that no test failed when the refusal's `continue` was deleted, because each refusal
  input was a file the rewrite left alone (4e67dd97 fixed the inputs).
