---
title: "A text body draws typed blocks through renderBlock and reads no server-owned attribute"
category: dispatch-web
tags:
  - typed-blocks
  - headless-engine
  - proof-editor
  - untrusted-input
  - bundle
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-526"
  - "sjawhar/legion#2310"
---

# A text body draws typed blocks through renderBlock and reads no server-owned attribute

To draw a document's chrome (a header, a card) for a typed block in a comment, ask or message
body:

- Draw it through the headless engine's `renderBlock` option, which becomes the node's `toDOM`.
  Do not override the `DOMSerializer`. The schema's `toDOM` wrapper keeps `data-proof-block-attr-*`
  and `data-block-id` on the drawing, and the parse rule reads only `[data-proof-block-content]`,
  so HTML copied from a body still pastes into a document as the same block. A serializer
  override drops those attributes.
- A body never passes the server's strip of server-owned attributes. The document service strips
  them only on document writes. Reset every attribute the block schema marks `server: true` to its
  default when the body is parsed, before anything draws. Drive the reset from the schema, never
  from a list of names. Otherwise a comment that types `state="answered" answered_by="…"` shows an
  outcome nobody recorded.
- A component that renders a `MarkdownBody` itself cannot be imported by the body's renderer:
  `AskBlockCard → AskOptionRow → MarkdownBody → useRenderedMarkdown` is a cycle, and Biome refuses
  it. Provide the component at the app root through a React context (`features/refs/markdown-cards.ts`).
  Keep the shared drawing in a module that imports only types from `prosemirror-model`
  (`features/doc/typed-block-drawing.ts`).
- Prove that the app root's provider did not pull the editor into the entry chunk. After
  `bun run build:web`, grep the entry chunk that `web/dist/index.html` names for a literal that only
  the editor module holds (`dispatch-ask-block-editing`, its `PluginKey`). Expect 0. As a positive
  control, find the same literal in a lazy chunk, so the 0 cannot come from minification.
- `packages/proof-editor/src/lib-headless.ts` is a copied file. Its allowed edits are a numbered
  list in `packages/proof-editor/AGENTS.md`. A new option there adds an entry and changes both
  counts, and a sibling change to the same file adds its own entry, so a merge sums the counts.

## Evidence

- `features/refs/markdown-engine.ts` passes `renderBlock` (an `ask` to `renderAskBlockShell`,
  every other type to `renderTypedBlock`). `src/tests/block-schema.test.ts` holds the case "a host
  drawing of a headless typed block keeps the block's copy attributes". Without the pass-through
  it fails, drawing the bare section.
- `withoutServerAttributes` in `markdown-engine.ts` performs the reset. With it skipped, the
  `MarkdownBody.test.tsx` case "an ask block in a body ignores server-owned attributes its text
  forges and draws an open decision" fails: `Expected: "open"`, `Received: "answered"`.
  Since INFRA-612 the drop runs in `remarkReadOnlyDirectives`
  (`packages/proof-editor/src/read-only-directives.ts`), after the schema accepts the block, and
  `withoutServerAttributes` is gone.
- The entry chunk held 0 occurrences of `dispatch-ask-block-editing` at every head of
  sjawhar/legion#2310. The literal was found in the lazy `SubscribedAgents-*.js` chunk each time.
- The merge with INFRA-529 (sjawhar/legion#2303, merge `f6a986e8`) brought the allowed-edit
  list to seventeen kinds, with both entries.
