---
title: "Skipping an unchanged write stales every value read from that row's timestamp"
category: envoy
tags:
  - write-skip
  - derived-values
  - change-detection
  - shared-bucket
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2377"
---

# Skipping an unchanged write stales every value read from that row's timestamp

When a writer stops rewriting a row that has not changed (for example a heartbeat that rewrites
it only once it is a few minutes old):

- Before the change, find every reader of that row's timestamp, not just the reapers. A displayed
  "last seen", a sort order or an age shown on a page now lags the truth by up to the refresh age.
  A unit suite does not show this; an end-to-end read of the consumer does.
- Do not fix a stale display by writing again. Report the value from the record that still updates
  on every event (here the session cache's last heartbeat), per row, next to the stored row. Have
  the consumer prefer it and fall back to the row's own timestamp when it is absent (a restarted
  process, an older build).
- Keep the source of that value for exactly as long as the consumer can still list the row. Compute
  the deletion allowance once, in one exported function, and call it on both sides, so the
  retention and the deletion cannot drift apart.
- Decide "would this write store what the row already holds" by comparing encodings, not fields.
  Stamp a copy of the current row with the new row's volatile field (its timestamp), marshal both,
  and compare the bytes. A field added later, and the difference between a null and an empty list,
  are then covered without a hand-kept list.
- When the stale value reaches a page a person reads, prove the fix in the browser suite. Make the
  two candidate values hours apart, so the rendered `<time dateTime>` shows which one the page drew.
  Run the test on a build before the fix and see it fail.

## Evidence

- `packages/envoy/internal/store/kv.go`, `UpsertWithin`: skips an unchanged row younger than
  `InterestRefreshAge` by comparing `json.Marshal` of the merged row with a copy of the current row
  stamped with the merged row's `UpdatedAt`. A field-by-field helper was replaced in review.
  `InterestReapAllowance` is the one allowance, used by `Reap` and by the listener's
  `session.WithLastSeenRetention` (`cmd/listener/main.go`).
- `packages/envoy/cmd/listener/api.go`, `interestRowFor`: `GET /v1/interests/` rows carry
  `last_seen` from the session cache. `packages/envoy/internal/dispatch/api/subscribers.go` uses it
  for a subscriber that is no longer live, else the row's `updated_at`.
- The tester of sjawhar/legion#2377 found the stale subscribers time after the skip had passed its
  unit tests. `packages/dispatch/e2e/issue.e2e.ts`, "a subscriber no longer live shows its last
  heartbeat, not its subscription's last write", passes at the fix and failed on a build before it,
  which drew the row's write time (`Received: ...10:57:23.596Z`, "3 hours ago").
