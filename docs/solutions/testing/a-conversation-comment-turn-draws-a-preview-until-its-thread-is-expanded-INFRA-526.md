---
title: "A Conversation comment turn draws a preview until its thread is expanded"
category: testing
tags:
  - dispatch
  - e2e
  - playwright
  - conversation
  - markdown-body
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-526"
  - "sjawhar/legion#2310"
---

# A Conversation comment turn draws a preview until its thread is expanded

Before you plan a proof on a Dispatch text surface, read which component draws it in its default
state. A browser test that reads the wrong one passes or fails for a reason that has nothing to
do with the change.

- A collapsed comment turn on `/issues/<KEY>/conversation` renders a two-line `MarkdownPreview`
  (`features/margin/ThreadCard.tsx`), which draws only inline content. The full `MarkdownBody`
  appears only after the turn's **Expand thread** button.
- On the `iphone` project, Expand thread opens a `role="dialog"` named "Thread", and the turn list
  behind it becomes `aria-hidden` and `inert`. On desktop the turn expands in place. So the
  locator root differs per project.
- Find the turn by `li[data-turn="comment:<id>"]` under the "Conversation turns" list, using the
  id `createComment` returns. A text filter breaks once the change adds words to the turn.
- Reuse `openCommentThread` in `packages/dispatch/e2e/text-typed-blocks.e2e.ts`. It expands the
  thread, waits for the dialog or the **Collapse thread** button, and returns whichever holds the
  body.

## Evidence

INFRA-526's plan cited `ConversationTab.tsx:737` as the comment turn that renders the full body.
That line is a targeted message turn. The implementer found this before the red run and wrote
`openCommentThread` (commit `6d878d04`). With it, the red test failed at the base on the header
and card locators for the right reason, and passed at the head on both projects.
