---
title: "Record a timeout only when your own deadline fired: use context.WithTimeoutCause"
category: database-issues
tags:
  - context
  - timeouts
  - metrics
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/api
---

# Record a timeout only when your own deadline fired: use context.WithTimeoutCause

- A bounded operation that records why it stopped must tell its own deadline from the caller going
  away. A request context ends when the client disconnects, and `ctx.Err()` then looks like a
  timeout.
- Make the bound with `context.WithTimeoutCause(ctx, d, sentinel)`. When the work stops, record
  `timeout` only if `errors.Is(context.Cause(bounded), sentinel)`; record a separate reason (here
  `cancelled`) for any other cause.
- Do not test `ctx.Err() == nil` instead. When the operation runs under an outer allowance that also
  uses the sentinel (several checks sharing one bound), the outer deadline is still "ours", and only
  the cause says so.
- Keep the new reason out of any count that means "skipped for time".

## Evidence

sjawhar/legion#2397's related-item check counted a client that disconnected mid-check as
`timeout`, which the records route reports as `skipped_for_time`, the figure a production
acceptance line reads (review round 1). The fix (fe5f66eb) is `errRelatedAllowanceEnded` and
`callerGone` in `packages/envoy/internal/dispatch/api/suggestions.go:146-151`, `:207`, `:371-385`.
On a local server, three `dispatch issue` runs killed two seconds into a held check recorded
`cancelled`, and one left waiting recorded `timeout`.
