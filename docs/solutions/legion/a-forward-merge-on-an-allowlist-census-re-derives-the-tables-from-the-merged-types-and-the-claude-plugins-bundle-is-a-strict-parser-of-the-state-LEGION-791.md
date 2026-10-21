---
title: "A forward merge on an allowlist census re-derives the tables from the merged types, and the Claude plugin's committed bundle is a strict parser of the state, not a copy of the number"
category: legion
tags:
  - daemon-api-contract
  - forward-merge
  - allowlist
  - claude-envoy
  - check-dist
  - contract-change-census
date: 2026-10-21
status: active
module: packages/daemon/internal/api
related_issues:
  - "LEGION-791"
  - "sjawhar/legion#2096"
---

# A forward merge on an allowlist census re-derives the tables from the merged types, and the Claude plugin's committed bundle is a strict parser of the state

Extends docs/solutions/legion/a-long-branch-loses-the-serial-number-it-reserved-INFRA-31.md.

That note gives the serial-number rule and the forward-merge discipline. The delta:

- A plan's contract number is an estimate with a shelf life. Before the first push, fetch the base
  and read `DaemonAPIVersion` there; the implementer re-derives the number and records the
  difference as a deviation in its handoff and the pull request, never as a silent edit of the plan.
- A forward merge on a census-shaped change re-opens the census, not only the serial. The base
  can add configuration fields the allowlist must classify and remove fields the plan's table
  named: re-run the classification over the merged types, decide each new field (emit or exclude
  with a reason), and drop the rows whose fields are gone. A test that derives its type set by
  walking the root type — rather than a hand list — makes this re-census the test's own failure
  message instead of a reading exercise.
- `packages/claude-envoy`'s committed `dist/` bundle is a fourth strict parser of the state
  document, not merely a second copy of the number: a new required member of `LegionStateResponse`
  migrates inside the bundle, and only `bun run build` on the pinned Bun moves it, held by
  `bun run check-dist` in the `Legion Envoy and Contracts` workflow for any `packages/contracts/**`
  change. Budget a contract bump as four declarers (`version.go`, `fixtures/daemon-api/version.json`,
  `pi-legion/package.json`, `claude-envoy/package.json`), one rebuild, and both CHANGELOGs; lint and
  typecheck cannot see a stale bundle.
- Re-run the same live predicate set at the pre-merge head, the merge head and the fix head, so the
  record shows the merge changed no behaviour: the fingerprint comment says the diff changed, the
  three identical runs say what did not.

## Evidence

sjawhar/legion#2096 (LEGION-791) was planned at contract 30; while the plan was written the base
took 30 through 34. GitHub reported the first push `CONFLICTING` (17 files); one forward merge
resolved it at 35 with both histories kept in `version.go`'s entry. The merge removed
`config.Project.Repo` (the plan's `projects.<KEY>.repo` row; the live proof's negative control was
the loader's refusal, `Unknown key "projects.FIXTURE.repo"`) and added `production_check_hours`,
`worker_backends`, `runtime.kubernetes.setup` and `runtime.kubernetes.repository_cache`, which the
derived type walk reported as unclassified until each had a row. The simplify pass's quality
reviewer, not CI, found `packages/claude-envoy/package.json` still at 34 with a bundle inlining the
pre-change strict schema; `bun run build` and `check-dist` closed it. The tmux real-binary proof
ran unchanged at 818b9654, defed47c (the merge) and 9b1a38ef.
