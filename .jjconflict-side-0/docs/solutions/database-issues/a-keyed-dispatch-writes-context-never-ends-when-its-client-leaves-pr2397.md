---
title: "A keyed Dispatch write's context never ends when its client leaves"
category: database-issues
tags:
  - context
  - idempotency
  - timeouts
  - dispatch
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2397"
---

# A keyed Dispatch write's context never ends when its client leaves

Extends docs/solutions/database-issues/record-a-timeout-only-when-your-own-deadline-fired-use-withtimeoutcause-pr2397.md.

- A handler of a POST or PATCH that carries an `Idempotency-Key` runs on
  `context.WithoutCancel(r.Context())`, bounded by `keyedHandlerLimit` (10 minutes). Its client
  leaving does not end it.
- So a reason recorded when the context ends for a cause that is not yours means "the client left"
  only for a write sent without a key. For a keyed write it means the keyed run's own bound ended
  first. Say both wherever the reason is described: code comment, migration comment, operator docs.
- A test that drives "the client left" must send its write without a key, or cancel the context it
  passes to the handler directly.

## Evidence

sjawhar/legion#2397 described its `cancelled` reason as the client leaving. After main's #2449
keyed every write (`packages/envoy/internal/dispatch/api/request_keys.go:218`, `:314`), review
round 12 found the description false for keyed writes. ca85bfa9 rewrote it in
`suggestions.go:146-152`, `0139_related_checks.up.sql:37-40` and `cmd/dispatch/README.md`.
