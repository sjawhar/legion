---
title: "React's onBeforeInput carries no input type, so an editor that needs the edit kind listens to the native beforeinput"
category: dispatch-web
tags:
  - react
  - textarea
  - beforeinput
  - editor
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/conversation
---

# React's onBeforeInput carries no input type, so an editor that needs the edit kind listens to the native beforeinput

- When editor logic depends on the kind of edit (`deleteByCut`, `insertFromPaste`, `deleteByDrag`, `insertFromDrop`, a deletion) or the range it replaces, attach the browser's own `beforeinput` with `addEventListener`, from a callback ref on the field. Do not use React's `onBeforeInput` prop.
- React 19's `onBeforeInput` is the legacy `textInput` event (react-dom's BeforeInputEventPlugin): its native event has no `inputType`, and no cut, deletion or Backspace fires it.
- Prove such logic with real keyboard cut and paste in a browser test. A unit test that dispatches synthetic events proves only the handler, not that the browser ever calls it.

## Evidence

sjawhar/legion#2463 tracked each block's span through a comment edit in `MentionComposer.tsx` (`block-spans.ts` `mapSpans`). The handler read `inputType` from `onBeforeInput`, and its unit tests passed. The Playwright row in `packages/dispatch/e2e/conversation.e2e.ts` that cuts a paragraph with Control+X and pastes it elsewhere showed the save carrying no id for the moved block: the handler had never seen `deleteByCut` or `insertFromPaste`. The fix (`bodyFieldRef` in `MentionComposer.tsx`) adds a native `beforeinput` listener; the unit test `a block cut and pasted elsewhere keeps its id in the edit's save` dispatches native `beforeinput` events and fails with the listener switched off.
