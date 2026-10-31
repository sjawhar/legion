---
title: "A cursor over ids skips nothing only for rows written under the lock that orders their commits"
category: database-issues
tags:
  - postgres
  - cursor
  - events
  - commit-order
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch
---

# A cursor over ids skips nothing only for rows written under the lock that orders their commits

- A sweep that reads `id > cursor` and then moves the cursor misses nothing only when every row is
  inserted under a lock that makes ids commit in id order. Dispatch's `events` no longer has one:
  sjawhar/legion#2392 removed the commit-order lock in `events.Broker.Append`, so a reader of
  `events` by id reads only up to `events.Horizon`'s safe horizon and never moves its cursor past
  it (`an-id-cursor-reader-of-a-table-whose-ids-are-drawn-before-commit-stops-at-a-proven-horizon-pr2392.md`).
  Write the dependency in a comment beside the read.
- For rows written outside that lock (a uuid id, a timestamp, a sequence taken before a slow
  commit), track "not judged yet" per row instead: a boolean column with a partial index
  `where not <flag>`, set false on insert and true by the sweep. Add it with a constant default
  `true`, then set the default `false`, so existing rows count as done and the table is not
  rewritten.
- A row judged together with others must also be judged against everything older than it, not
  only against what came after the cursor: it may have committed after a later-started row the
  sweep already read.

## Evidence

Pull request sjawhar/legion#2391, the suggestion outcome sweep
(`internal/dispatch/api/suggestion_outcomes.go`, `judgeSuggestionPage`). The plan's cursor on
`write_suggestions.id` could not work: ids are uuids and suggestions are inserted after their
write commits. Migrations `0153_suggestion_outcome_cursor` (event cursor) and
`0154_write_suggestions_swept` (the per-row flag and its partial index) replaced it. After #2392
the event cursor also stops at the safe horizon (`suggestionOutcomePass`).
