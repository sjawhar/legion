---
title: "A check that passes on a missing failure marker also needs proof the guarded path ran"
category: testing
tags:
  - evidence
  - absence
  - production-check
  - dispatch-search
date: 2026-10-28
status: active
module: scripts/dispatch
---

# A check that passes on a missing failure marker also needs proof the guarded path ran

A narrower case of the testing learning "Evidence can be absent rather than wrong, and absence
reads exactly like success".

- When a check passes because a failure marker is missing (a `degraded` field, an `error` key, a
  warning line), read the producer for every early return that answers before the marker could
  be set. Each one is an answer without the marker that proves nothing.
- Require positive evidence that the guarded path ran, such as a non-empty result. A missing
  marker without that evidence is `unavailable`, never `pass`.
- The check's input is the operator's, so the rule cannot rely on the example input the
  documentation shows. Put an input that takes the early return in the negative controls.

## Evidence

- Dispatch's search handler counts the tsquery nodes of `q` first. A query of stopwords only
  answers `{results: []}` with no `degraded` field, before it checks for an embedder at all
  (`runSearch`, `packages/envoy/internal/dispatch/api/search.go:422-428` on main). It does this
  also on a Dispatch with no embedder.
- At bc359229, `judgeA1` in `scripts/dispatch/meaning-search-check.ts` passed any answer without
  `degraded`, so `--a1-query 'the'` passed on that Dispatch. 186e6b46 judges an empty answer
  without the field as `unavailable: no-results` (`judgeA1`, line 313 at 7e042950). On
  production `--a1-query 'the'` then printed `A1 degraded=no results=0 unavailable: no-results`
  and exited 2.
