---
title: "An error code a client retries without limit names only a state that ends on its own"
category: integration-patterns
tags:
  - retry
  - error-codes
  - dispatch-web
  - tanstack-query
date: 2026-10-30
status: active
module: packages/dispatch/web/src/api
related_issues:
  - "sjawhar/legion#2392"
---

# An error code a client retries without limit names only a state that ends on its own

- Before a client retries an error code without limit, check every server path that emits that code. The code must mean only a condition that ends without anyone acting, such as a write that will commit or a proof that will arrive.
- Give every other failure (a database or pool that does not answer, a bug) a code of its own. The client retries that code on its ordinary bounded rule, which ends at a visible error with a Retry button.
- Test both ends: on the server, the self-ending condition answers the unlimited code and a failed dependency answers the other one; in the client, the other code stops after the bounded retries. Each test must fail when the two codes are merged back into one.
- Put the client's rule in the default retry policy (`queryRetry` passed to the query client in `main.tsx`), not in one query's `retry` option. A per-query `retry` overrides the policy of every client that renders that query, tests that set `retry: false` included.

## Evidence

In sjawhar/legion#2392, `authWhoami` (`packages/envoy/internal/dispatch/routes/router.go`) first answered `503 EVENT_HEAD_UNAVAILABLE` for every `eventHead` error, and `queryRetry` (`packages/dispatch/web/src/api/query-client.ts:32`) retried that code without limit. So a database outage would have left the page on its loading skeleton for ever, with no error and no Retry. Commit `185bb85d5936` answers that code only for `errors.Is(err, api.ErrEventHeadUnproven)` (`router.go:480`) and answers `EVENT_HEAD_READ_FAILED` otherwise (`router.go:484`). It is proven by `TestWhoamiAnswersEventHeadUnavailableOnlyWhileNoHeadIsProven`, a unit test of `queryRetry`, and `packages/dispatch/e2e/shell.e2e.ts:93`.

The retry first sat on `whoAmIQuery` itself (`005075ed`). That broke the unit test "a failed identity refetch keeps the cached user's sidebar preference" in CI, because the test's client set `retry: false`. Commit `629fe0ee` moved the retry into the default policy.
