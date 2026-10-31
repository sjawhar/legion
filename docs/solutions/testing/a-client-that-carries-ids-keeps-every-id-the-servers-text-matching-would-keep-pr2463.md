---
title: "A client that carries ids keeps every id the server's text matching would keep"
category: testing
tags:
  - block-ids
  - editor
  - oracle
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/conversation
---

# A client that carries ids keeps every id the server's text matching would keep

Extends docs/solutions/testing/test-a-pure-matcher-against-generated-edit-histories-not-one-database-test-per-edge-case-pr2233.md.

- When a client carries ids through an edit and the server also has a path that matches by text without ids, use the text path as the oracle: for each edit kind (select-and-type, paste over a selection, cut and paste, drag and drop, Backspace join, Enter split), the client's mapping must not drop an id the text rules would keep.
- Test the client's pure mapping function (here `mapSpans` in `block-spans.ts`) against those edit kinds, then one browser row per kind the unit table cannot reach.

## Evidence

In sjawhar/legion#2463 the dashboard editor dropped a cited paragraph's id when its whole text was selected and typed over: the replaced range covered the span, so `mapSpans` removed it, while the same body sent without ids kept the id through `Align`'s rewording rule. The tester's browser row in `conversation.e2e.ts` states that oracle in its comment. The fix keeps the span when one edit replaces the whole text of exactly one span with new words; typing over two whole blocks still removes both (`block-spans.test.ts`).
