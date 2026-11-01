---
title: "A word diff over user content is bounded by edit count and memoised on the texts, never on the control that reveals it"
category: dispatch-web
tags:
  - performance
  - diff
  - react
  - memoisation
date: 2026-10-31
status: active
module: packages/dispatch/web/src/features/doc
related_issues:
  - "sjawhar/legion#2549"
---

# A word diff over user content is bounded by edit count and memoised on the texts, never on the control that reveals it

A Myers diff is quadratic in the edit distance, and a "block" of a document can be a whole table or
a fenced code block, so two unrelated blocks are a main-thread stall of seconds. Two rules, and the
measurements that set them belong in the code beside the bound.

- **Bound it.** Pass `maxEditLength` and treat the undefined return as the whole of the old side
  removed and the whole of the new side added. That is also what a reader of two unrelated blocks
  wants: the old block struck through beside the one that replaced it, rather than a pairing of
  words that share nothing.
- **Memoise it on the data.** Compute the diff where the two texts are paired, not where a row is
  rendered. A control that reveals more rows - a fold, a page, a filter - re-enters the render path,
  and a diff computed there is paid again for every pair already on screen, which no user action
  explains.

State in the module what is still unbounded, so the next reader knows which half was measured.

## Evidence

`packages/dispatch/web/src/features/doc/change-set.ts`: `MAX_WORD_EDITS = 800` (`:87`) with
`changedWords` returning the whole-block pair when `diffWordsWithSpace` gives `undefined` (`:96`-`:103`),
and the comment carrying the numbers measured in a Legion issue pod: 1000 words 0.7 s, 3000 words
7.7 s, 6000 words 39 s, bounded about 110 ms. Block pairing itself (`diffArrays`) is still unbounded,
as the `diffLines` of the `VersionDiff` this replaced was, and the pull request says so.

The memo half was a review finding of round 2, fixed in `1f9799b3`: `ChangeView.tsx` built its rows
with `useMemo(..., [entries, opened])` and `buildRows` called `changedWords` for every changed entry
it drew, so each "Show N unchanged blocks" click diffed every changed pair again. Each pair now
carries its words from the `entries` memo, which is keyed on the two documents alone. The unit row
spies on `changedWords` and counts 3, 3, 3 across an initial render and two fold openings, where it
counted 3, 6, 9 before; one 3000-word pair costs 136 ms, measured in the pod.
