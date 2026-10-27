---
title: "Bun's TextEncoder miscounts a long string with a non-ASCII Latin-1 character, and one computed row keeps an exported test table in every bundle"
category: build-errors
tags:
  - bun
  - textencoder
  - utf-8
  - byte-bound
  - bundling
  - tree-shaking
date: 2026-10-26
status: active
module: packages/contracts
related_issues:
  - "INFRA-366"
  - "sjawhar/legion#2210"
---

# Bun's TextEncoder miscounts a long string with a non-ASCII Latin-1 character, and one computed row keeps an exported test table in every bundle

- Never count a byte bound that TypeScript shares with Go using `new TextEncoder().encode(s).length`
  under Bun. Count one code point at a time, as Go's `len` does (`utf8Bytes` in
  `packages/contracts/src/dispatch-text-rules.ts`). Pin it with a test row longer than 1024
  characters that holds an `é`.
- Write every row of an exported test table, one that a test in Go or TypeScript imports, as
  literals. A computed element (a `repeat()` call, or a template string over an imported constant)
  is code with possible side effects, so Bun can keep the whole array in a bundle that imports any
  other export of the module. Here that bundle is `packages/claude-envoy/dist`, which is shipped.

## Evidence

- **TextEncoder:** under Bun 1.3.14, `new TextEncoder().encode("a".repeat(1100) + "é").length` is
  1101, while `Buffer.byteLength` gives 1102 (checked at this retro). INFRA-366's
  256-byte and 2048-byte bounds were counted with it at first. aee29df2 moved them to `utf8Bytes`,
  and `dispatch-text-rules.test.ts` holds the long-string row.
- **The test table:** the long block id rows of `DISPATCH_TEXT_REFERENCES`
  (`packages/contracts/src/dispatch-text-references.ts`) were first built with
  `` `${"%C3%A9".repeat(MAX_BLOCK_ID_BYTES / 2)}` ``, which imported `MAX_BLOCK_ID_BYTES` into the
  table's module. 9d4fb04e ("the shared text table's long-id rows written out so a bundle leaves
  the table out") wrote them out as literals and dropped the import. The comment above those rows
  says why.
