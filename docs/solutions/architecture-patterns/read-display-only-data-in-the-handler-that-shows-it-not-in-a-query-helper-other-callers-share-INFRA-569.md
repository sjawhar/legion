---
title: "Read display-only data in the handler that shows it, not in a query helper other callers share"
category: architecture-patterns
tags:
  - dispatch
  - search
  - shared-helper
  - write-path-latency
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-569"
  - "sjawhar/legion#2363"
---

# Read display-only data in the handler that shows it, not in a query helper other callers share

- Before you add an extra read (another query, a lookup of related rows) inside a function, list
  every caller of that function. If any caller drops the new field, do the read in the handler
  that shows it instead. Otherwise that caller pays for the round trips, and a failed read becomes
  a new way for it to fail.
- This matters most when a write path shares the helper under a short deadline. In Dispatch,
  issue and ask creation compute their related-item suggestions through `runFusedSearch` within
  `writeSuggestionTimeout`.

## Evidence

- INFRA-569's plan attached each ask hit's answer state (`attachAskStates`) inside
  `runFusedSearchQuery` and `runMeaningSearch`, so that "the search handler and
  `computeSuggestions` both carry the state". The write-time suggestions share
  `runFusedSearchQuery`, but `suggestionFromResult` (`api/suggestions.go`) builds each suggestion
  field by field and drops `Ask`. So every issue or ask create with an ask among its hits paid one
  or two extra queries, and a failed read would have dropped that write's suggestions.
- The simplify pass (284986e6) moved the call into the search route's handler (`search` in
  `api/search.go`), with `TookMS` still counting the read. INFRA-568 calls `attachAskStates` from
  its own related-item check, where it shows the state.
