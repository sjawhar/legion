---
title: "Skip a repeated parse only where it is the same parse, and fuzz the skip against the read it stands for"
category: envoy
tags:
  - goldmark
  - pmdoc
  - performance
  - fuzzing
  - read-back
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2427"
---

# Skip a repeated parse only where it is the same parse, and fuzz the skip against the read it stands for

- When a value is parsed again under a second rule set, first list every line where the two
  parsers' configurations branch. A skip is exact only where none of those branches can take a
  different path on this input. Then the second parse is the first one, step for step.
- Where the difference is one decision per token, compute that decision under both rule sets
  during the first parse. Record any disagreement as a key in that parse's own `parser.Context`
  (`parser.NewContextKey()`), never in package state, since the parsers are shared across
  goroutines. Return it to the caller with the nodes.
- Where the difference is confined to one character, gate the skip on that byte being absent from
  the whole source the parser reads, including anything appended to it (footnote definitions).
- Keep the caller's verdict order: when the readers are tried in order and stop at the first
  failure, return the count of readers passed, so every verdict stays the same.
- Commit a fuzz target that, wherever a skip would apply, runs the skipped read anyway and
  requires the same nodes, the same error text and the same divergence flag. Add a test that the
  flag is false for ordinary input (a flag stuck at true passes the fuzz test but saves nothing),
  and one render test per skip that fails when the skip is forced.
- When main changes the rule sets, prove the argument again. The proof is per configuration.

## Evidence

- Three readers (`inlineMarkdown`, `storedMarkdown`, `flankingMarkdown`). storedRules and
  flankingRules differ only at `emphasis.go:117`; editorRules adds the `~` widening at
  `emphasis.go:125`, `tildeParser`, `tildeDelimiters` and the tildeRuns group in
  `delimiterList.push`, all of which need a `~`.
- The flag: `flankingDivergenceKey` (`emphasis.go:90`), set when a run's stored pair differs from
  its flanking pair after `underscoreRule` (`emphasis.go:151`); returned by
  `parseInlineWithDefinitions` (`parse.go:291`). The gate: `holdsTilde` (`render_inline.go:306`).
  The count: `readsBackUnder` (`render_inline.go:272`).
- `FuzzReadBackSkips` (`flanking_test.go:14`) ran 5 minutes clean. With the flag forced false,
  `TestRenderReadsBackUnderEveryReaderASkipCouldMiss` (`flanking_test.go:99`) fails on `a*_*`.
  With the stored read skipped although a `~` is present, it fails on `a*~*`.
- The first design assumed two readers. Main's #2360 made them three and forced a redesign: the
  equal open/close pair no longer implied an equal parse once `~` runs paired in a pass of their
  own (commit 7edefc89).
- Read-back CPU inside a render fell from 30.6 s to 9.8 s over 10 renders of the LINUX corpus
  (PR body, "Render cost").
