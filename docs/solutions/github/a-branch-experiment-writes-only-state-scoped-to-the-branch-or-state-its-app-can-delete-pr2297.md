---
title: "A branch experiment writes only state scoped to the branch, or state its App can delete"
category: github
tags:
  - github-actions
  - ghcr
  - registry
  - experiments
  - github-app-permissions
  - cleanup
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2297"
---

# A branch experiment writes only state scoped to the branch, or state its App can delete

1. Before a branch run writes to anything outside the branch (a registry tag, a package version, a
   shared cache key), check that a run on `main` cannot read what it writes. Prefer state GitHub
   scopes to the branch: an Actions-cache entry written on a branch is never restored by a `main`
   run.
2. When the experiment must write shared state, first check that your role's App can delete it.
   The implement App cannot delete GHCR package versions
   (`DELETE users/<owner>/packages/container/<package>/versions/<id>` answers
   `403 Resource not accessible by integration`), so a tag it writes stays until a person removes
   it.
3. Never write under a name that `main` reads, even once, to measure something: what the branch
   wrote is then what `main`'s next run consumes.

## Evidence

- To measure a registry-cache hit, branch run 40453177184 (temporary commit `17fa916a`) wrote
  `ghcr.io/sjawhar/legion/envoy:buildcache-amd64` and `buildcache-arm64` (package versions
  1452720428 and 1452457314). Until review thread PRRT_kwDORFy7ds6wucBM, those were the tags
  `main`'s first attested build would have imported.
- The implement App's delete of both versions got 403. Both tags stay in the public package; the
  PR body and `packages/envoy/AGENTS.md` record that nothing reads them.
- Round 2 measured its cache hit in the branch's own Actions-cache scope instead (temporary
  commit `d2605357`), which a `main` run never restores.
