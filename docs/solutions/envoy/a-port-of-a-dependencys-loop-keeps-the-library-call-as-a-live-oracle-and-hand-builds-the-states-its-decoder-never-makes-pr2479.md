---
title: "A port of a dependency's loop keeps the library call as a live oracle and hand-builds the states its decoder never makes"
category: envoy
tags:
  - differential-testing
  - fuzzing
  - ygo
  - pmdoc
  - test-oracle
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2479"
---

# A port of a dependency's loop keeps the library call as a live oracle and hand-builds the states its decoder never makes

Extends docs/solutions/envoy/replace-a-deep-copy-with-copy-on-write-by-keeping-the-old-pipeline-as-a-test-oracle-and-finding-every-writer-pr2427.md.

When you copy a third-party library's loop into your own code (to skip a lock, an intermediate
value or an allocation), the library keeps changing and your copy does not:

- **Keep at least one production caller on the library's own call**, and commit a test that runs
  both readers on the same inputs on every CI run. A dependency upgrade that changes the library's
  behaviour then fails that test, where a copied-out oracle in a `_test.go` file would change with
  nothing.
- **Name the library file, function and exact version in the port's doc comment, and list each
  rule of the loop you kept.** The reviewer checks the port against that version line for line,
  and the next upgrade knows what to compare.
- **List every state the loop branches on, then ask which of them the library's writers, its
  decoder and your fuzzer can actually produce.** Cover them in three tiers: scripts through the
  library's own API; crafted update bytes the decoder accepts but no writer emits; and item lists
  linked by hand for states no decoded update can hold (a typed-nil value, an empty string between
  formats, a deleted format item). Keep each hand-built case as a named table row. It may be the
  only test that reaches that branch.
- **Where the oracle itself is nondeterministic, relax the comparison only there.** An error that
  depends on Go map order passes when the port's text is one the oracle gives in repeated reads.
  Compare trees by a canonical dump: marks sorted by their own dump where the production sort
  leaves ties in arrival order, and `%#v` so NaN prints and never needs `DeepEqual`. Everything
  else stays an exact match.

## Evidence

- `pmdoc/read.go:186` `readUnsharedText` ports `YText.ToDelta` (sjawhar/ygo v1.51.3-sami.4); its
  doc comment lists the four rules kept. `ReadUnshared` (`:31`) uses it for the cold fold, while
  `readLiveText` (`:55`) still calls `text.ToDelta()` for `Read`.
- `pmdoc/read_unshared_test.go:30` `readersDiffer` is the oracle. `:59` `readRefuses` re-reads
  with `Read` up to 200 times for the map-order case, and `:72` `readerDump` is the canonical dump.
- `:388` `itemsDoc` links items by hand. `:418` `itemsCases` holds 19 rows, among them `:439`
  `deleted items are skipped`. The tester found that row was the only lock on a walk that reads
  deleted format items: neither fuzzer nor 15,000 generated Yjs histories produced one.
- ygo decodes a crafted empty `ContentString` without error but never integrates it, so the
  empty-string rule is reachable only through hand-built item lists. A walk that merged across an
  empty string failed `items/an_empty_string_between_formats_ends_the_run`.
