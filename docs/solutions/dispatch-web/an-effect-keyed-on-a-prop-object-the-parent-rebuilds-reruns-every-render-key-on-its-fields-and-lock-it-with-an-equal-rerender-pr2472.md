---
title: "An effect keyed on a prop object the parent rebuilds reruns every render: key on its fields and lock it with an equal rerender"
category: dispatch-web
tags:
  - react
  - useEffect
  - remount
  - unit-tests
  - flaky-tests
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/doc
related_issues:
  - "sjawhar/legion#2472"
---

# An effect keyed on a prop object the parent rebuilds reruns every render: key on its fields and lock it with an equal rerender

- Before an effect that builds something costly (an editor, a socket, a scroll) lists an object
  prop in its dependencies, check whether the parent builds that object in its render body. If it
  does, the effect tears down and rebuilds on every parent render.
- Key the effect on the object's scalar fields instead: read each field into a local, and
  `useMemo` the object from those fields, so Biome's `useExhaustiveDependencies` stays satisfied
  and the effect depends on the memo.
- Lock it with a unit case, not with browser repeats: render, then `rerender` with a new object
  equal to the first, and assert the costly thing was built once and not destroyed; then
  `rerender` with different fields and assert a second build. A browser row sees the rebuild only
  as a timing symptom and catches it a fraction of the time.

## Evidence

In sjawhar/legion#2472, `VersionView`'s editor effect listed `highlight`, which
`ArtifactDocument`/`ProofDocument` built afresh on each render. A past version opened by a comment
link built its editor 2-4 times per load; the page shrank between builds, and the last
`focusMark` could run before the last editor had laid out, leaving the reader at the top. The
browser rows failed 5 of 40 runs at the round-1 code. The fix (`VersionView.tsx`, a memo over
`by`/`id`/`quote`) and its lock (`VersionView.test.tsx`, "keeps its editor when the parent renders
again with an equal highlight") landed in f3f2427a and ffb93ba0. Negative control: the memo keyed
on the object again → `Expected length: 1, Received length: 2`.
