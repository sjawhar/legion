---
title: "A selector that outlives a collaborative edit is kept outside the editor and matched against block offsets"
category: dispatch-web
tags:
  - prosemirror
  - anchors
  - collaborative-editing
  - text-quote-selector
date: 2026-10-31
status: active
module: packages/proof-editor
related_issues:
  - "sjawhar/legion#2548"
---

# A selector that outlives a collaborative edit is kept outside the editor and matched against block offsets

A record attached to a passage the reader never selected is worse than a refusal, because no
refusal ever tells them about it. Everything below follows from that.

- **Keep nothing in the editor that has to survive a remote change.** A tracked ProseMirror range
  does not: y-prosemirror 1.3.7 applies every remote update as one
  `tr.replace(0, doc.content.size, …)` (`sync-plugin.js:629`), which collapses every mapped range,
  and a reconnect rebuilds the editor and its plugin state outright. Keep what you will need where
  the feature's own state lives — here the composer's anchor, which also rides the held-send store
  through a reload.
- **Keep a text-quote selector, not a position and not an index.** The words, a bounded amount of
  the text on each side as the editor read it at selection, and the id of the block it was in. An
  index into "the readings of these words" is not enough: another writer prepending a reading of
  the same words moves the reader's own from 1 to 2, and the comment lands on a passage they never
  chose.
- **A side kept short is a constraint, a side never kept is none.** `prefix: ""` because the words
  began their block means "this must sit at a block's start"; no placement at all means "nothing is
  known". Code that cannot tell those apart matches everything. Derive the distinction from the
  data (`prefix.length < CAP` ⇒ the block edge was reached) and let the no-filter value say
  explicitly that it constrains neither side.
- **Match against block offsets, never against a character in the flattened text.** The document
  read as one text (`textBetween(0, size, "\n", "\n")`) spells a hard break, every line of a fenced
  code block, a horizontal rule and the separator between blocks with the same `"\n"`. No scan of
  that text can recover structure. Return the block offsets beside the text (`{at, base, length}`)
  and decide every edge, and every offset-to-position mapping, from them.
- **One function reads the text that a captured count and a later match both rest on.** A gate of
  the form "the block held these words once then and holds them once now" is only meaningful while
  both counts come from the same scan over the same text. Two hand-kept copies that "obviously
  agree" are a latent gate failure.
- **Say how wide the context is, and why.** 32 characters each side here, chosen by measuring this
  repository's own prose: of 13,876 phrases that read more than once inside one block, 32
  characters separate 90.4% of them, against 81.1% at 16 and 53.0% at 4. A unit test pins the cap
  against a shorter one that would not separate two readings a review found.

## Known limit

A remembered prefix that itself begins with `"\n\n"` can still align onto a horizontal rule's
offset, because that rule reads as a newline of its own in the flattened text. Matching is against
block offsets, so the reading chosen is still inside a block; the residue is that such a prefix
constrains less than it looks as if it does.

## Evidence

sjawhar/legion#2548 (`packages/proof-editor/src/record-mark-retype.ts`,
`packages/dispatch/web/src/features/margin/margin-context.tsx`) recovers a comment whose
provisional mark the server has swept, another writer has removed, or a tab offline past the
mark's lifetime has lost. It reached that shape through four review rounds, each of which found a
case the previous round's fix made reachable:

1. A tracking plugin kept the composer's range through transactions. It could never answer in
   production, for the y-prosemirror reason above, and was deleted.
2. A remembered reading index took the comment to a passage the reader never selected as soon as
   another writer added an earlier reading in the same block.
3. An empty remembered side matched any text, so a selection of a whole heading, list item, table
   cell or short paragraph told no reading from another: two readings elsewhere were called alike,
   and a lone unrelated reading silently took the comment.
4. A block edge decided by `text[prefixAt - 1] === "\n"` accepted a line inside another paragraph
   or a code block, which is the same defect as 3 in a different coordinate system.
