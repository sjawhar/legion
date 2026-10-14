---
title: "A Dispatch web test queries its own render and unmounts it: the preload registers no testing-library cleanup, so one happy-dom document is shared across files"
category: testing
tags:
  - testing-library
  - happy-dom
  - test-isolation
  - bun-test
  - dispatch-web
  - ci-file-order
date: 2026-10-14
status: active
module: packages/dispatch/web
related_issues:
  - "LEGION-573"
  - "sjawhar/legion#1944"
symptoms:
  - "a web test passes alone and in its file, and fails in CI's `dispatch` job"
  - "`screen.queryByTestId(...)` finds an element the test's own component never rendered"
  - "a test asserting an element is absent fails because another file's render is still mounted"
---

# A Dispatch web test queries its own render and unmounts it: the preload registers no testing-library cleanup, so one happy-dom document is shared across files

`packages/dispatch/web/src/__tests__/setup.ts` registers happy-dom once (`GlobalRegistrator.register()`)
and no `afterEach(cleanup)`: its one `afterEach` resets the ask-reply drafts. Every file of the
`bun test` run renders into that one `document.body`, and a render a file never unmounts stays in
it for every file that runs after.

## The rule

- Query the render you made: `const card = within(view.container)` and `card.getByText(...)`,
  never `screen` or `document.body`, for an element your own component renders or must not
  render. A `screen` query is a cross-file order dependency: it fails on CI's file order, not
  yours.
- Unmount what you rendered, in a `finally`, so the next file starts from the document you
  started from (`view.unmount()`; `VersionDiff.test.tsx` and the approval-card tests in
  `AskCard.test.tsx` do this).
- A file that keeps `screen` pairs it with its own `afterEach(cleanup)`; do not add a global one
  to the preload, since the files written against the shared document depend on its absence.
- Read the per-job result of CI, not the aggregate: the red that found this was CI's `dispatch`
  job while the branch's other lanes were green, and the round was reported green until the
  tester read the job.

## Reproducing a cross-file leak

A stray element left in the shared document reproduces the CI failure without the other file:
a throwaway preload (`bun test --preload ./web/src/__tests__/setup.ts --preload <stray>.ts`) that
appends the element to `document.body` turns the four tests red in the same four places CI
showed, and green once they query their own container.

## Tests that pin it

`AskCard.test.tsx`: "a full/compact approval card names the earlier approval and diffs the two
versions only when asked" (both variants), "a first approval request shows no earlier approval and
no diff control", "the diff cuts every decision block, so answers alone leave nothing changed",
"the diff keeps a paragraph changed beside the decision blocks"; `VersionDiff.test.tsx`.

## Evidence

sjawhar/legion#1944 (LEGION-573), round 2: CI's `dispatch` job failed four new `AskCard` tests at
`0282d514` on `expect(view.queryByTestId("version-diff")).toBeNull()` and on diff text, because
`VersionDiff.test.tsx` left a `version-diff` element mounted and ran first in CI's order. The
fix (`3c0c5df9`) scoped the queries to `within(view.container)` and unmounted both files'
renders; the full `packages/dispatch` suite (2,253 tests) passed before and after the merge of
main in the same round.
