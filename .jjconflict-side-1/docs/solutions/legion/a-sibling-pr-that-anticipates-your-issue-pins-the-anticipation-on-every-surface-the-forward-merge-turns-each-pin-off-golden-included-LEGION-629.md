---
title: "A sibling pull request that anticipates your issue pins the anticipation on every surface: the forward merge turns each pin off, the cross-language golden included"
category: legion
tags:
  - forward-merge
  - anticipation
  - golden-fixture
  - cross-language
  - capabilities
  - conflict-resolution
date: 2026-10-09
status: active
module: packages/daemon/internal/capabilities
applies_when:
  - A pull request that merged into main while your branch lived names your issue key in a "once X lands" or "awaits X" sentence, status value or field
  - You are forward-merging main into the branch that is X
  - The daemon's wire shape changed and a fixture under packages/contracts/fixtures is pinned by a Go test in one package and parsed by a bun test in another
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
  - "sjawhar/legion#1846"
---

# A sibling pull request that anticipates your issue pins the anticipation on every surface: the forward merge turns each pin off, the cross-language golden included

Extends docs/solutions/legion/long-lived-branch-mechanics-jj-new-and-merge-not-rebase.md.

- A tree that lands while yours lives may have written *your* change into `main` as a not-yet: a
  status value (`installed` where `present` is the landed state), a field whose only use is the
  not-yet (`Capability.Awaits`), a sentence naming your issue key in a Go doc comment, a test name,
  a test comment, a Dockerfile comment, docs prose, and the wire shape's golden fixture. At your
  merge head every one of those is false. The forward merge's conflict list shows only the files
  both sides edited; the pins sit in files your branch never touched, which jj takes from `main`
  without a marker.
- Before the merge commit is pushed, grep the merged tree for the issue key with **no path and no
  extension filter** — `grep -rn 'LEGION-629' --exclude-dir=node_modules --exclude-dir=.jj .` —
  and for the vocabulary of the anticipated state (the status word, the field name, "once", "until",
  "awaits"). A grep narrowed to `--include=*.go` or to the package you changed finds the code pins
  and misses the JSON fixture, the TypeScript literal and the prose; a pin that names the state and
  not the issue (`CheckImage renders it installed`) is found only by the vocabulary grep. Give every
  hit a disposition in the merge commit: the sentence goes, the mechanism may stay for a later row
  (an empty `Awaits` with a test that no row uses it), the test is re-aimed at the landed state.
- The daemon's state golden is cross-language and cross-package:
  `packages/daemon/internal/api/state_golden_test.go` writes
  `packages/contracts/fixtures/daemon-api/state.json` and fails when the fixture differs from what
  Go now renders; `packages/contracts/src/legion-api.test.ts` parses that fixture through the strict
  schema and pins row values by literal. A change to anything `populatedState()` renders — the
  capability table, `Deployment.Report`, any `api.State` field — stales the fixture while the
  package you changed stays green, since the Go test is in `internal/api` and the bun test in another
  package. Regenerate with `go test -run TestStateGolden ./internal/api/ -args -update` (the flag
  is the test binary's: after `-args`, not before the package path), then re-aim the bun test's
  literals and run `bun test packages/contracts/src/legion-api.test.ts`. Both are in CI's `test`
  job; a pod lane that names only the packages it changed never runs them.

## Evidence

sjawhar/legion#1848 (LEGION-629) forward-merged `main@origin` f1dcadfe, which carried #1846
(LEGION-578's capability report). #1846 had anticipated LEGION-629: `capabilities.go` declared
`codeGraphAwaits = "a pod's agent gets the codegraph tool once its launch loads profile plugins
(dispatch://LEGION-629)"` on the CodeGraph row, `image.go` and `deployment.go` rendered a row with a
non-empty `Awaits` as `installed` instead of `present`, four Go tests pinned `installed`
(`TestOnlyTheCodeGraphRowAwaitsAPodLaunch` among them), the golden `state.json` carried the sentence
as the row's `detail`, `legion-api.test.ts` pinned it by literal and asserted `installed` among the
fixture's statuses, and the Dockerfile header, plugin-install step, publish-gate comment and two
`docs/kubernetes.md` passages said "once LEGION-629 lands". The merge commit 24f2532c turned off the
Go pins (the row's `Awaits` is `""`, the constant deleted, the four tests re-aimed at `present`) and
the comments, found by a grep restricted to `--include=*.go --include=*.md --include=*.sh
--include=Dockerfile packages/daemon docs/kubernetes.md scripts/e2e AGENTS.md`. That grep excluded
`packages/contracts`, so the golden and the bun test kept `installed`; `go test` on the changed
packages was green, and CI's `Tests` run 37864230699 at 24f2532c failed `TestStateGolden`
(`state.json is stale`, got `present` where the fixture said `installed`). 7d98d70e regenerated the
fixture with `-args -update`, re-aimed the bun test and `docs/kubernetes.md:917`; `Tests` 37865275948
passed. The reviewer's round still found one pin the vocabulary grep would have caught and the
issue-key grep cannot: `image.go:226-228`'s doc comment "the row's Awaits says what that still
needs, and CheckImage renders it installed", true of no row at that head (`review.json`
`keyFindings[0]`, Minor, fast-follow). The retro's own grep for `installed` across the tree is what
listed it.
