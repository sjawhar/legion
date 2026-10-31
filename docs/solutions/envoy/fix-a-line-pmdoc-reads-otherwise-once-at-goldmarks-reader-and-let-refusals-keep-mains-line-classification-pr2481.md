---
title: "Fix a line pmdoc reads otherwise once, at goldmark's reader, and let refusals keep main's line classification"
category: envoy
tags:
  - pmdoc
  - goldmark
  - refusals
  - browser-parity
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# Fix a line pmdoc reads otherwise once, at goldmark's reader, and let refusals keep main's line classification

Extends docs/solutions/envoy/lifting-a-pmdoc-refusal-accepts-only-the-shape-proven-to-read-alike-and-rewrites-every-go-claim-about-the-browser-pr2403.md.

- When goldmark gets a line's geometry wrong, fix it once, in the reader every parse uses
  (`lastLineColumns` wraps goldmark's `text.Reader` in `parseSource`). Examples of such geometry:
  a last line with no line feed, or the columns a container leaves of a tab. Override `PeekLine`,
  `Advance`, `AdvanceToEOL` and `AdvanceLine` together. Do not patch each block parser's text: a
  fix in one consumer moves the line under the others.
- Before you call such a fix done, grep every block parser for a guard on the same geometry. An
  example is `offset < len(line)`, which skips the last line when it has no line feed. Each one is
  a second copy of the rule.
- When a line's text must change, change what it contributes to the text. Leave the segment in
  the node's `Lines()`, and mark it with an attribute, as `fenceEndsEmptyAttr` does for
  `fencedCodeText`. Every blank-line counter in `source_lines.go` reads segments to decide which
  lines are code. Removing a segment changes what those counters see.
- A change can reclassify lines: a line that was blank becomes the code's text (held lines), or
  goldmark's reader now serves a line it used to drop. Then give each refusal that counts blank
  lines the old classification explicitly (`asHeldBefore`, `lastLineKeptAttr`). Otherwise a
  refusal you never edited stops refusing. The documents it let through can then store a misread
  that is unrelated to your change and was already on main.
- Keep one predicate for a shape that both the reader and a refusal decide on, and make the
  refusal call it (`heldOpenBy`, used by `codeColumns.Close`, `interruptsOpenBlock` and
  `refusals.go`). If one copy is edited and the other is not, the guarantee that nothing stored is
  newly refused breaks, and no test fails.

## Evidence

- Round 2 (sjawhar/legion#2481, 2781d3d5) fixed a last line with no line feed in two places:
  `codeBlockText`, and `fenceClosure`, by clearing `ForceNewline`. That regressed 27 documents
  where a quote already took the blank line (tester round 2, red rows in 3a32ac88). Round 3
  (c076c8c4) moved the rule into `lastLineColumns`.
- Review round 1 still found a third copy of the rule, `directive.go`'s `&& offset < len(line)`
  (fixed in dd60d85f).
- Removing the empty fenced segment made the line blank to `blankInside`, so the head newly
  refused 56 documents that main read correctly. Keeping the segment and marking it fixed this.
- Held lines stopped counting as blank in "a typed block holding a blank line inside a list item".
  The head then stored 1,000 documents main refused. Each holds `- :::ask{…}` on a list marker
  line, which Go reads as a typed block and the engine as text, on main as well.
  `asHeldBefore` and `lastLineKeptAttr` brought the differential against main back to 0 on every
  GATE line.
