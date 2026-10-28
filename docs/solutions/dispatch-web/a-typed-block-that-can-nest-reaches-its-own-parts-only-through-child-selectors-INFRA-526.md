---
title: "A typed block that can nest reaches its own parts only through child selectors"
category: dispatch-web
tags:
  - css
  - selectors
  - typed-blocks
  - decision-block
  - markdown-body
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-526"
  - "sjawhar/legion#2310"
---

# A typed block that can nest reaches its own parts only through child selectors

Typed blocks nest in Dispatch. A text body (`MarkdownBody`) is drawn inside a decision card's
footer: a reply, an answer note, an option's description. A decision can also sit inside another
decision's option. So a rule or a lookup from a block to one of its own parts must step to a
direct child, never to any descendant.

- In CSS, write `.proof-typed-block-ask > [data-proof-block-content] > ul`, never
  `.proof-typed-block-ask [data-proof-block-content] > ul`. Use the same `>` for the fold, the
  margins and every other rule that keys on the block's own content hole.
- In the DOM, find a shell's slots with `shell.querySelector(":scope > [data-dispatch-ask-footer]")`.
  A bare `querySelector("[data-dispatch-ask-footer]")` walks in document order. A nested block's
  footer sits inside the content hole, so it comes before the outer shell's own footer.
- Recheck every such selector when a change gives nested content a marker the outer block's
  rules already key on, such as a `[data-proof-block-content]` hole in a body that used to draw
  a bare section. The old selectors were correct only because nothing nested carried the marker.
- To write a nested container directive in a test or fixture, give the outer block a longer
  fence: `::::ask` around `:::ask`. With two `:::` fences, the inner closing `:::` also closes the
  outer block, so the test parses a different structure from the one it means to test.

## Evidence

- The decision's fold rule in `packages/dispatch/web/src/styles.css` used a descendant
  combinator. After INFRA-526 gave every typed block in a body a content hole, a document
  decision hid the bullet list of a callout in a reply shown in its own card. The tester's test
  `e2e/text-typed-blocks.e2e.ts:137` (commit `49082939`) failed at `b6720fc1` (`step one`
  resolved hidden) and passes at `9439f4a8`, after the four rules took `> [data-proof-block-content]`.
- `collectAskHosts` in `features/refs/useRenderedMarkdown.tsx` found its slots with a
  descendant lookup, so an outer decision's card mounted into a nested decision's footer. The
  `MarkdownBody.test.tsx` case "a decision written inside another decision's option keeps each
  card in its own block" failed at `9439f4a8` (expected 2 outer option rows, got 0) and passes at
  `a1faafec`, where both slot lookups (`useRenderedMarkdown.tsx`, `features/doc/ask-block.ts`
  `slot()`) take `:scope >`.
  Since INFRA-612 both lookups are `askShellSlots` in `features/doc/typed-block-drawing.ts`.
- The reviewer's first body for that case used `:::ask` for both blocks and did not nest. The
  committed case uses `::::ask`, as `e2e/unknown-directive.e2e.ts` and
  `features/doc/decision-blocks.test.ts` already did.
