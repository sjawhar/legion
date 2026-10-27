---
title: "A proof-editor paste is tested through handlePaste with the HTMLElement global, on an empty line, and against stored markdown pinned from a run"
category: testing
tags:
  - proof-editor
  - prosemirror
  - paste
  - happy-dom
  - playwright
date: 2026-10-27
status: active
module: packages/proof-editor
related_issues:
  - "INFRA-565"
  - "sjawhar/legion#2388"
---

# A proof-editor paste is tested through handlePaste with the HTMLElement global, on an empty line, and against stored markdown pinned from a run

- **Unit tests paste through `packages/proof-editor/tests/paste.ts`.** `pastePlainText` calls
  `view.someProp("handlePaste", …)`, which is what a browser paste reaches, because happy-dom
  delivers no paste event to ProseMirror's own listener. Wrap it in `withHTMLElementGlobal` when
  the pasted markdown holds a heading or a table: their `parseDOM` checks `instanceof HTMLElement`
  during the paste's DOM round trip, and without the global the paste throws
  `HTMLElement is not defined`. Leave it off only for a paste that must throw for another reason.
- **To compare the headless engine with the live editor, remove what each mints.** Drop every
  `blockId` and a heading's `id`: the live editor's heading-id plugin fills `id` from the heading
  text, and the headless engine leaves it empty.
- **A browser paste row presses Enter before it pastes.** A plain-text paste opens its slice at
  both ends, so the first pasted paragraph joins the paragraph the caret is in; pasting on an
  empty line keeps the stored markdown's blocks the ones the row names.
- **Pin the stored markdown from the first run at the head, never from what you expect the
  serializer to write.** Go's `Render` escapes only a line that would not read back as itself, so
  what it escapes depends on what precedes the line. Then check that the pinned string keeps every
  word and reads back as the document the row means.

## Evidence

- `pastePlainText` and `withHTMLElementGlobal` in `packages/proof-editor/tests/paste.ts`;
  `tests/clipboard-paste-recovery.test.ts` pastes bare where it needs the throw and wrapped for its
  table.
- `withoutMintedIds` in `packages/proof-editor/tests/typed-block-drawing.test.ts` strips the
  heading `id`; without it the two engines' documents differed only there.
- `openOnEmptyLine` in `packages/dispatch/e2e/typed-block-content-rule.e2e.ts` presses Enter; the
  heading row pins
  `Intro text.\n\n\\:::ask{#a1}\n\n# Heading words\n\n:::\n`. The plan expected the closing `:::`
  escaped; after the escaped opening line, Go stores it plain, and a lone `:::` paragraph reads back
  as text (`packages/envoy/internal/dispatch/pmdoc/escape_test.go`, the `":::"` row).
