---
title: "Replacing a micromark construct fuzzes old against new with every construct its lines meet"
category: integration-patterns
tags:
  - micromark
  - proof-editor
  - fuzzing
  - corpus-fixtures
date: 2026-10-28
status: active
module: packages/proof-editor
---

# Replacing a micromark construct fuzzes old against new with every construct its lines meet

Extends docs/solutions/envoy/goldmark-insertafter-of-a-last-child-counts-it-twice-and-a-parser-harness-counts-panics-apart-and-varies-endings-INFRA-172.md.

- When you replace a construct, use the old one as the oracle. Compare the mdast of the old and new
  engines over generated documents, and count throws separately from differences.
- The generator's alphabet includes everything the new construct's continuation, lazy lines and
  exits meet across lines, inside list items and quotes at several prefixes:
  - fenced code (` ``` ` and `~~~`, opened and left open);
  - HTML flow (`<div>`, `<!--` left open);
  - indented code;
  - lazy paragraph lines;
  - footnote and link reference definitions;
  - a document that ends without a final line feed.
- Commit each difference you keep as a `pmdoc/testdata/corpus` fixture that fails on the old
  construct (a throw in `bun run gen`, or `TestParseMatchesMilkdownForFixtures` failing). Name each
  residual difference class in the construct's header comment and in the changelog.
- The old package can leave the dependency tree with the change. Keep a copy of its construct
  where the fuzzer can import it until the new one is settled.

## Evidence

- The first fuzzer for pull request #2403 generated only containers, blank lines and directives,
  and reported 0 differences. The review found two defects it could not reach:
  - a block's exit placed inside a fenced code block that crosses the gap, which made
    mdast-util-from-markdown throw, so the comment showed as raw text;
  - the line after a closing fence read as lazy, which split indented code in two.
  Both were fixed in 3d9fd4e9, with three fixtures that fail on the previous construct.
- Once fences, HTML flow and indented code were in the alphabet, 3 × 60,000 documents gave 0 new
  throws.
