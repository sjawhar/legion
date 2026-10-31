---
title: "A guard that weighs what a write would delete renders the whole unit and rebuilds from its own children"
category: envoy
tags:
  - dispatch
  - pmdoc
  - refusal
  - write-validation
  - marks
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2525"
---

# A guard that weighs what a write would delete renders the whole unit and rebuilds from its own children

- **Decide whether a write would delete a person's content by rendering both sides and comparing
  the markdown**, never by reading a node's marks or its types. What a reader can lose is what the
  document writes; everything else is bookkeeping the browser editor leaves behind.
- **Never write a second list of which marks are invisible.** `pmdoc` holds one
  (`pmdoc/marks.go`, `renderedMarkTypes`: `link`, `strong`, `emphasis`, `strike_through`,
  `inlineCode`), and `Render` writes nothing for any other. A guard with a list of its own counts
  the editor's `proofAuthored`, which the browser puts on every character a person types, and a
  comment's or a suggestion's anchor, as content, and then refuses an edit while quoting a line
  that reads plain.
- **Render the whole unit the markdown writes, not the text block inside it.** A list item's
  `checked` is written by the item, not by its paragraph (`pmdoc/render.go`, `[x] ` / `[ ] `), and
  it feeds `pmdoc.CountTasks`, which is the issue's `tasks_done`/`tasks_total`
  (`docs/task_progress.go`): weighing the paragraph alone let an agent's edit drop a ticked option
  and change a count on another object, unseen.
- **Take out of the comparison only the attribute under test.** Build the other side with the
  unit's own attributes less that one, so the attributes a rebuilt structure carries anyway
  (`label`, `listType`, `spread`) stand on both sides and blame nothing.
- **Build the "nothing but its own text" side from the node's own inline children**, keeping each
  text node's text and each break node as it stands with the marks stripped, and leaving every
  other inline node out so a write over an image or a footnote reference is weighed as the
  deletion it is. Do not rebuild it from `pmdoc.TextContent`: that reads every break back as
  `"\n"`, while the renderer writes a soft break as a space and drops a trailing line feed, so a
  rebuilt comparison turns a break a person typed into markup their line does not hold.
- **A trimmed or parsed value is never a side of the comparison.** Compare the stored node with a
  rebuild of its own text, never with a rebuild of what some parser read out of it: a parse that
  trims and cuts (here `Label: Description`) makes whitespace a person typed look like markup, and
  the refusal then quotes `&#32;` at a caller who cannot see what it is accused of keeping.

## Evidence

sjawhar/legion#2525 rewrote this one guard three times under review, in
`packages/envoy/internal/dispatch/docs/ask_option_merge.go` and `ask_block_text.go`:

1. It read marks (`plainInline`). Every option a person had typed in the browser was refused a
   drop, a rename or a `--clear-options`, with a quote that reads plain.
2. It compared the item's rendering with a rendering of the **parsed** option. A line reading
   `Hold: Wait for review ` or `Hold : Wait` was then refused as markup it does not hold.
3. It compared the **line** with a paragraph of that line's own text. A soft break and a trailing
   line feed were refused the same way, and the item's `checked` was no longer weighed at all, so
   an options edit dropped a ticked option and the issue's progress fell from `tasks 1/1` to
   `none`.

The rule above is what stands: `rendersTheSame` over `renderBlock`, `holdsOnlyItsOptionLine`
comparing item with item under the item's own attributes less `checked`, and `textAndBreaksOf`
building the text side from the node's own children. Each failure is locked by a test that fails
without it (`TestAskOptionsEditDropsAnItemTheEditorMarked`,
`TestAskOptionsEditOfALineHoldingTypedWhitespace`, `TestAskOptionsEditOfALineHoldingATypedBreak`,
`TestAskOptionsEditRefusesDroppingATaskOption` in `docs/service_test.go`), and each was driven on
the real `dispatch` CLI against a Dispatch server built from the head that had the bug, which is
how the third one was caught on the issue's own progress line rather than in the document alone.

`checked` is hard-coded as the one list-item attribute the markdown writes as content. If
`pmdoc`'s list renderer ever writes another, drops of real content start being accepted again and
only `TestAskOptionsEditRefusesDroppingATaskOption` stands in the way.
