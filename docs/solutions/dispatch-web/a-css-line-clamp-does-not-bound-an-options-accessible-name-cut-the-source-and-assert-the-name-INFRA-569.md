---
title: "A CSS line clamp does not bound an option's accessible name: cut the source and assert the name"
category: dispatch-web
tags:
  - accessibility
  - accessible-name
  - line-clamp
  - markdown
  - playwright
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-569"
  - "sjawhar/legion#2363"
---

# A CSS line clamp does not bound an option's accessible name: cut the source and assert the name

- `line-clamp` (and `MarkdownPreview`'s `lines`) only hides text visually. Everything rendered
  inside a `role="option"`, a button or a link with no `aria-label` is still in the DOM and is
  its accessible name. An unbounded field there (a person's answer, a comment body) becomes a
  name a screen reader reads from start to end.
- Bound such a field before it renders: cut the source to a named constant, and balance the cut
  so no Markdown delimiter is left open (`markdownHead` in
  `web/src/features/refs/markdown-fragment.ts`, which calls `balanceCutMarkdown`).
- Prove the bound in the browser, on the accessible name: seed a source far over the bound that
  ends in a word nothing else holds, then assert
  `toHaveAccessibleName(/^[\s\S]{1,N}$/u)` and `not.toHaveAccessibleName(/<that word>/u)`. A check
  of the source's length does not prove it, because references render as their titles and the name
  differs from the source.

## Evidence

- INFRA-569's palette status line first passed the whole answer text to a three-line
  `MarkdownPreview`. The reviewer found that the full answer, up to about 1 MiB, became the
  option's name (review thread `PRRT_kwDORFy7ds6wkC-L` on sjawhar/legion#2363).
- Fixed in b9278db5. `SearchPalette.tsx` passes `markdownHead(answer.text, ANSWER_PREVIEW_CHARS)`
  to `MarkdownPreview`. `e2e/search.e2e.ts` answers with more than 2,000 characters ending in
  `zenithward` and asserts that the option's name starts `ask `, does not hold `zenithward`, and is
  under 600 characters.
