---
title: "A Milkdown parse that stops throwing needs a content-rule check, because Milkdown drops a block that breaks its rule silently"
category: dispatch-web
tags:
  - milkdown
  - prosemirror
  - proof-editor
  - read-only-rendering
  - content-rule
date: 2026-10-27
status: active
module: packages/proof-editor
related_issues:
  - "INFRA-529"
  - "sjawhar/legion#2303"
---

# A Milkdown parse that stops throwing needs a content-rule check, because Milkdown drops a block that breaks its rule silently

When a change turns a throwing parse into a forgiving one, the fallback that the throw used to
trigger goes away. Then look for input that is now lost without any error.

- Milkdown's `ParserState` builds each node with `NodeType.createAndFill`. When that returns
  null (the children break the node's ProseMirror content rule), it logs
  `createNodeInParserFail` with `console.error`, throws nothing, and drops the node with all of
  its text. A heading inside a `paragraph+` block, or paragraphs after the bullet list of an
  `ask` (`paragraph+ bullet_list?`), disappear from the document.
- Before such a parse keeps a typed block, check that its children fit, with the library's own
  builder and the parser's own matching:
  - Map each mdast child to the node type whose `spec.parseMarkdown.match` accepts it, as
    `ParserState` does.
  - For a child that matches a mark type, use that child's own children in its place: the mark's
    runner opens the mark and runs them into the same parent.
  - Call `type.createAndFill(defaultAttrs, placeholders)` and keep the block only when the result
    is not null.
  - Decide blocks innermost first. A block drawn as text changes what its parent holds, so check
    the parent again against its own rule.
- Unit-test with the production block schema (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`),
  not a one-type toy schema. A toy schema has no `ask` rule and no `dispatchAsk` span, so a test
  that uses one cannot see this loss.
- Assert that the text of every source line is in the output, and replace `console.error` with a
  stub for the parse and assert nothing was logged. A dropped block and a good parse both return
  a document without an error.

## Evidence

- `packages/proof-editor/src/read-only-directives.ts:48-77`: `placeholders`, with the mark case at `:55`, and `fitsContentRule`, with the `createAndFill` call at `:76`; `:145-157` is the innermost-first decision.
- The tester's red test (commit `ee272bb6`) used an ask that holds a list and then an unknown
  `:::note`. Round 1's read-only parse rendered only `Lead` from it. Round 1's unit tests used a
  `callout`-only schema and passed.
- Review round 1 found that a block-level `<span data-dispatch>` wraps the following blocks in a
  mark node. The first fit check treated that child as fitting, and the ask still vanished
  (`createNodeInParserFail … heading(dispatchAsk("Heading words"))`). Fixed in `75cd715e`.
- `packages/proof-editor/tests/read-only-directives.test.ts:282` loads the server schema.
  `:401-424` stubs `console.error` for the byte-order-mark row.
- Related rules: `docs/solutions/integration-patterns/a-value-a-library-consumes-is-validated-with-that-librarys-own-parser-read-back-whole-INFRA-271.md`
  (check with the library's own rule) and
  `docs/solutions/testing/evidence-can-be-absent-rather-than-wrong-and-absence-reads-exactly-like-success-INFRA-31.md`.
