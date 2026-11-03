---
title: "An editable editor never hides an element because it looks empty: the caret sits in it"
category: dispatch-web
tags:
  - prosemirror
  - caret
  - css
  - read-only
  - typed-blocks
date: 2026-10-28
status: active
module: packages/dispatch
related_issues:
  - "INFRA-612"
  - "sjawhar/legion#2405"
---

# An editable editor never hides an element because it looks empty: the caret sits in it

- Do not hide an element of editable editor content because it is empty. A selector such as
  `li:empty`, `li:has(> p:empty)` or `li:has(> p > br:only-child)` also matches the block a person
  is typing into. ProseMirror draws an empty block that holds the caret as
  `<p><br class="ProseMirror-trailingBreak"></p>`. When that block is hidden, the next keystrokes
  go into the nearest visible block.
- Hide an empty element only under a container that nobody edits. In Dispatch that is
  `.dispatch-markdown` (every comment, ask, message and reply body) and
  `.dispatch-doc[data-read-only="true"]` (a closed issue's document, or the version view). Keep the
  hiding in `styles.css` under those containers, not in a class the editor's node view applies
  everywhere.
- When a surface copies a rule from another surface ("hide it in a body, as the document does"),
  check that the copied rule is correct on its first surface in every mode, editable included.
- Prove the editable case with the keyboard: type through the real editor, then read the stored
  markdown back through the API. A `display` assertion on a fixture that was seeded already blank
  does not exercise the caret.

## Evidence

- Before `00db1b8a`, a malformed decision's content class in
  `packages/dispatch/web/src/features/doc/typed-block-drawing.ts` held
  `[&_li:has(>p:empty)]:hidden [&_li:has(>p>br:only-child)]:hidden`, and the node view applies that
  class in the editable editor too. In a decision with no options, Enter then `- ` made an empty
  option. The decision became malformed (an option without a label), the class hid the option that
  held the caret, and `Option A` was stored as `Which one?Option A\n\n- \n`.
- The fix is `packages/dispatch/web/src/styles.css:450-456`, scoped to
  `:is(.dispatch-markdown, .dispatch-doc[data-read-only="true"])`.
- `packages/dispatch/e2e/text-typed-blocks.e2e.ts:330-350` types the option and checks the stored
  markdown. At main `2df3353c` it fails in chromium and iphone. At `00db1b8a` it passes.
- The tester widened the scope back to every `.dispatch-doc`, and an existing blank option in an
  editable document then hid again: Expected `"list-item"`, Received `"none"`.
- This PR's first version added `li:empty` to the same class so that a body would hide a blank
  option "as the document does". The reviewer of round 3 found that the document's own behaviour
  was the defect.
