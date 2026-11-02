---
title: "A count both sides raised by one merges clean and one short: recount it from the merged list"
category: legion
tags:
  - forward-merge
  - conflict
  - guides
  - counts
date: 2026-11-01
status: active
module: packages/proof-editor
---

# A count both sides raised by one merges clean and one short: recount it from the merged list

## The rule

- A guide line that states a count of the items listed under it ("Nineteen files under `src/`
  are legion's own") is derived from that list. When your branch and main each add one item and
  each raise the count by one, both sides make the identical edit. A three-way merge applies it
  once and reports no conflict, so the merged count is one short.
- After every merge that touches such a list, count the merged list and correct the number in the
  merge commit, whether or not the line conflicted.
- Count from the merged file, not from either side: `awk` over the table rows, or a grep of the
  list's entries, between the count's line and the list's end.
- Check every other count in the same file the same way. One whose side main left alone keeps your
  side's value, and that is correct.

## Evidence

- Merge `6ad11876` of main `ed9113e7` into sjawhar/legion#2394: this branch had added
  `src/block-pointers.ts`, and main's #2549 had added `src/editor-change-decorations.ts`. Each had
  changed "Eighteen files under `src/` are legion's own" to "Nineteen".
- The merge reported a conflict only in the Biome exclusion list further down the file. It kept
  "Nineteen" with no conflict, but the merged table has 20 rows (`packages/proof-editor/AGENTS.md`),
  so the merge commit changed the line to "Twenty".
- The same file's "Twenty-four kinds of edit" was right as merged: main had left it at
  twenty-three, so only this branch's edit applied.
