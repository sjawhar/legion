---
title: "A change that moves code greps the docs for the old file's line citations and for claims the move makes false"
category: legion
tags:
  - documentation
  - docs-solutions
  - refactor
  - review-rounds
date: 2026-10-27
status: active
module: docs/solutions
related_issues:
  - "INFRA-565"
  - "sjawhar/legion#2388"
---

# A change that moves code greps the docs for the old file's line citations and for claims the move makes false

Extends docs/solutions/legion/a-change-that-retires-a-path-or-mechanism-greps-the-old-string-repo-wide-not-the-diff-LEGION-636.md.

A move retires no name, so that document's grep for a retired string finds nothing. What the move
retires is every `<old file>:<line>` citation, and every sentence that says the moved code is not
yet shared or not yet changed.

- **Grep the whole tree for the old file's line citations:** `grep -rnE '<old-file>\.ts:[0-9]' docs packages skills`.
  Do it again for every test file whose helpers you moved. Point each hit at the new file and its
  new lines, and read each hit's whole bullet.
- **Read every document that cites the moved code as a claim, not as a link.** A bullet such as
  "default mode still passes `file.toString()` unchanged" names no line and goes on being cited
  after the change makes it false. Grep for the function and file names the change touched in
  `docs/solutions/`, and rewrite what is now false.
- Run both at the end of implementing and after each round that moves more code.

## Evidence

- INFRA-565 moved INFRA-529's fit check, `drawDirective` and the walk out of
  `packages/proof-editor/src/read-only-directives.ts` into `typed-block-drawing.ts`. Implementing
  updated one INFRA-529 solutions document and missed its sibling,
  `docs/solutions/integration-patterns/cut-source-text-by-mdast-offsets-from-the-text-micromark-counted-INFRA-529.md`.
  That document still cited `read-only-directives.ts:189`, `:108-118`, `:28` and `:121-135` in a
  file that was then 25 lines. It also said default mode's `remarkTypedBlocks` "still passes
  `file.toString()` unchanged", which this change reversed. Review round 1 of sjawhar/legion#2388
  found it; the fix is `d4771a07`.
- The round-2 helper move into `tests/paste.ts` and `tests/block-schema-fixture.ts` changed the
  lines of `tests/read-only-directives.test.ts` that the other INFRA-529 document cited
  (`:282`, `:401-424`). `grep -rnE 'read-only-directives\.test\.ts:[0-9]' docs` found them in the
  same round.
