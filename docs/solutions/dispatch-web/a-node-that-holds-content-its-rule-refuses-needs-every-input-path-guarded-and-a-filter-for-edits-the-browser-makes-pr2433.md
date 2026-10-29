---
title: "A node that holds content its rule refuses needs every input path guarded, and a filter for edits the browser makes"
category: dispatch-web
tags:
  - prosemirror
  - contenteditable
  - execcommand
  - filtertransaction
  - input
date: 2026-10-29
status: active
module: packages/proof-editor
related_issues:
  - "sjawhar/legion#2433"
---

# A node that holds content its rule refuses needs every input path guarded, and a filter for edits the browser makes

Once a node can hold content its rule refuses, an editor command that reads the rule
(`contentMatchAt`, `canReplace`, `dropPoint`) throws inside it. ProseMirror's DOM handlers do not
catch the throw. The throw comes before `preventDefault`, so the browser applies its own DOM edit,
and ProseMirror's read-back of that DOM drops blocks the person never selected.

- Guard every input path, not only keydown: `handleKeyDown`, `handleTextInput`, `beforeinput`,
  `cut`, `compositionstart` and `handleDrop`. Each guard tries the editor's own edit and declines
  when it throws. Dictation, mobile keyboards and IME commits send no keypress.
- Some edits cannot be cancelled before they happen, such as `document.execCommand`, which writing
  extensions use. Refuse them in a `filterTransaction`. When it refuses, the state stays unchanged,
  the DOM observer redraws the page from that state, and nothing is written to Yjs.
- Scope the filter to local transactions whose every step replaces a range inside the broken node.
  Exempt the history plugin's transactions and the CRDT binding's own transactions, else undo, redo
  and remote updates are refused.
- Judge "a block was dropped" from the step mapping, never from the resulting text:
  `tr.mapping.mapResult(at + 1, -1).deletedBefore` for each leaf (text, image, hard break, or a
  leaf block such as a divider) of each block outside the selection. A text search passes a block
  whose words another block repeats. Any mapping change also refuses joins, inserts and attribute
  sets.

## Evidence

- `packages/proof-editor/src/ask-content.ts:141` (`brokenAskInputGuards`), `:211`
  (`dropsUnselectedAskBlock`, the `deletedBefore` test at `:237`), `:267`
  (`keepUnselectedAskBlocks`).
- Round 1 had only the keydown guard. Review rounds 2 to 5 added the other paths (`b3a04795`).
  They replaced a text-based judgement with the mapping (`b5ba307b`) and extended it to every
  leaf and to a divider block (`d47ad385`).
- The Chromium rows `document.execCommand("insertText"|"delete")` in
  `packages/dispatch/e2e/ask-concurrent-edit.e2e.ts` fail when the plugin is removed.
