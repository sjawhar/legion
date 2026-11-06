---
title: "Every Milkdown markdown parse path needs the content-rule check, registered after the mark plugins and locked by a valid block"
category: dispatch-web
tags:
  - milkdown
  - prosemirror
  - proof-editor
  - remark
  - content-rule
  - plugin-order
date: 2026-10-27
status: active
module: packages/proof-editor
related_issues:
  - "INFRA-565"
  - "sjawhar/legion#2388"
---

# Every Milkdown markdown parse path needs the content-rule check, registered after the mark plugins and locked by a valid block

Extends docs/solutions/dispatch-web/a-milkdown-parse-that-stops-throwing-needs-a-content-rule-check-because-milkdown-drops-a-block-that-breaks-its-rule-silently-INFRA-529.md.

That document ties the silent drop to a parse that stops throwing. The drop needs no such change:
a parse that never threw for content (the editor's default mode) drops the block just the same.

- **List every markdown parse path, and rule out the ones that never parse markdown.** In
  Dispatch the markdown paths are a plain-text paste (`@milkdown/plugin-clipboard` parses with
  `parserCtx`), `setMarkdown` (a past version), the headless engine's default mode and its
  read-only mode. An HTML paste uses ProseMirror's own slice and Yjs content arrives through
  y-prosemirror; neither is a markdown parse, so a markdown fix does not reach them. Each markdown
  path needs the check, whether or not a throw was ever removed from it.
- **Reuse the check, do not copy it.** `remarkDecideTypedBlocks(schema, accepts)` in
  `packages/proof-editor/src/typed-block-drawing.ts` is the walk; `accepts` names which directives
  may stay typed. Keep that module free of imports from `block-schema.ts`, which imports
  `markdownSource` from it.
- **In the live editor, read the ProseMirror schema when the transformer runs.** Milkdown builds
  `remarkCtx` before the schema, so a `$remark` plugin that needs it calls `ctx.get(schemaCtx)`
  inside the transformer, never when the plugin is built.
- **Register a step that reads mdast after every plugin that rewrites the shape it reads.** A
  block-level `<span data-dispatch>` is raw `html` until `remarkDispatchMarksPlugin` makes it a
  mark node, so the fit check goes after the mark plugins.
- **Lock the order with a valid input.** An invalid block fails the fit check in either order (an
  `html` child fails it too), so it cannot tell the orders apart. A valid ask whose question sits
  in a block-level mark span passes at the base and fails only when the step runs too early.
- **Draw rather than refuse.** A throw in `handlePaste` does not mean "nothing changes":
  `prosemirror-view`'s `editHandlers.paste` calls `preventDefault()` only when `doPaste` returns
  true, and a throw in `handlePaste` escapes both before that, so the browser's own paste goes on.
  A load such as `setMarkdown` has no edit to refuse, and Dispatch has no channel to show the
  person a refusal. Drawing the block as its literal lines keeps every word on every path.

## Evidence

- `packages/proof-editor/src/lib.ts` registered `unfitTypedBlocksRemarkPlugin` right after
  `remarkDispatchMarksPlugin`; `buildParser` in `packages/proof-editor/src/lib-headless.ts` chains
  `remarkUnfitTypedBlocks` after `remarkTypedBlocks` in its default-mode branch.
- `remarkDecideTypedBlocks` and `unfitTypedBlocksRemarkPlugin` (the `$remark` wrapper reading
  `schemaCtx` per parse) were in `packages/proof-editor/src/typed-block-drawing.ts`.
- The ordering lock was the test "the live editor checks an ask's content after the mark plugins
  have run" in `packages/proof-editor/tests/typed-block-drawing.test.ts`, on `MARK_SPAN_VALID_ASK`.
  With the step moved before the mark plugins, it is the one row that fails (15 pass, 1 fail); the
  row that pastes an ask whose heading sits in a block-level mark span passes either way.
- At the base `b08a56bc`, pasting `:::ask{#a1}` / `# Heading words` / `:::` left the live editor
  holding only `Intro text.` and an empty paragraph
  (`packages/dispatch/e2e/typed-block-content-rule.e2e.ts`, run against a tree of the base).

**Note 2026-10-27 (sjawhar/legion#2433):** `unfitTypedBlocksRemarkPlugin` is gone. The live editor's step is
now `liveTypedBlocksRemarkPlugin` in `packages/proof-editor/src/read-only-directives.ts`,
which makes the read-only parse's drawing decision (`remarkDecideTypedBlocks` with the shared
`acceptedTypedBlock`) against the editor's schema (read on the first parse) and keeps the `server`
attributes, which `remarkReadOnlyDirectives` drops. It is still registered in
`src/lib.ts` right after `remarkDispatchMarksPlugin`, and in place of `typedBlockRemarkPlugin` too,
so the live editor also draws a container directive the block schema refuses as its lines rather
than throwing. The ordering lock is now the test "the live editor decides typed blocks after the
mark plugins have run". The headless default-mode branch is unchanged.
