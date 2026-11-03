---
title: "Port micromark's coupled delimiter rules together, and answer its event questions from the tokens your parser produced"
category: integration-patterns
tags:
  - micromark
  - goldmark
  - pmdoc
  - strikethrough
  - emphasis
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "INFRA-534"
  - "INFRA-535"
  - "sjawhar/legion#2360"
---

# Port micromark's coupled delimiter rules together, and answer its event questions from the tokens your parser produced

When you make a goldmark-based reader (pmdoc) read delimiter runs as micromark does:

- **Port the classification and the pass separation in one change.** micromark's
  `attentionMarkers` (GFM adds `~`) decide whether a `*`/`_` run can open or close beside a `~`.
  Its separate strikethrough and attention resolvers decide what pairs with what. Each half alone
  misreads input that goldmark's single loop read correctly by accident. Before you split a pass,
  pin those rows as tests, and do not leave the other half to another issue.
- **Answer "was the previous event X?" from what your parser consumed,** not from the raw bytes
  before the cursor. micromark's "a `~` run after an escaped `~`" asks about a character-escape
  event. A backslash that a bare URL took is not one. Count backslashes only within the text node
  goldmark's inline loop appended before it called your parser.
- **Record micromark's resolver order when the tokenizer is entered,** before any early return. A
  `*`/`_` run that pairs with nothing still counts as tokenized, and the kind tokenized first is
  paired first at a textblock's top level.
- **Take over goldmark's pairing by starving its list,** not by forking it. Push runs to a list
  of your own in the parser context, so `ProcessDelimiters` always meets an empty list. Then pair
  at goldmark's own hook points: the block's `CloseBlock`, and link formation, with goldmark's
  label bottoms mirrored at every `[`/`![` and `]`. Keep goldmark's single loop as one mode of the
  same code, so the old readers stay available (see
  `docs/solutions/envoy/a-writer-that-checks-its-output-by-reading-it-back-keeps-stored-bytes-only-when-the-new-reader-joins-every-step-of-the-old-ladder-INFRA-534.md`).

## Evidence

- The plan put the pass separation in INFRA-534 and the `~` marker in INFRA-535. Round-1 testing
  found `*~~**b~~*` read as `*~~*` then emphasis `b~~`, where main and the engine read emphasis
  around struck `**b`. Under the flanking rules the `**` after `~~` can only close, so the `*`
  pass paired it with the outer `*`. Counting `~` as a marker (e2dae381, `emphasis.go`
  `emphasisParser.Parse`) lets that `**` both open and close, so the rule of three keeps it apart.
  The rows are in `strikethrough_test.go`, `TestTildeRunsReadAsTheEngineReadsThem`.
- `escapedTilde` (`emphasis.go`) replaced a source-wide backslash count after the differential
  flagged `> ~~www.a.com/~x**~\~~~`. The `\` ends the bare URL there, and micromark refuses the
  run.
- `emphasisParser.Parse` calls `delimiters.tokenized(emphasisRuns)` before its "can neither open
  nor close" return. The order rows (`see www.a.com/~x* ~~a*b~~c*`, `[^a_b] ~~a*b~~c*` with and
  without a definition) pin the order.
- The takeover is `delimiters.go` (`delimiterList`, `pair` with an explicit stack),
  `emphasisParser.CloseBlock`, and `imageNesting.Parse` with `openLabel`/`closeLabel`/`pairLabel`
  (`nesting.go`).
