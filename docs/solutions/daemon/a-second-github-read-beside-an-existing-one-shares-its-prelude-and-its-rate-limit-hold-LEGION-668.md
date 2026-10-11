---
title: "A second GitHub read beside an existing one shares its prelude and its rate-limit hold, or the hold goes missing"
category: daemon
tags:
  - github-rate-limit
  - review-permission
  - review-body
  - workflow-runtime
  - code-review
date: 2026-10-10
status: active
module: packages/daemon/internal/daemon
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A second GitHub read beside an existing one shares its prelude and its rate-limit hold, or the hold goes missing

- `workflowRuntime` holds one GitHub rate-limit memory for every read it makes with the review
  App's token: `rateLimitLeft()` is the gate before a read and `holdRateLimit()` the record after
  one that met the limit. It is runtime-wide, not per endpoint: a read of `/pulls/{n}/reviews/{id}`
  and a read of `/collaborators/{login}/permission` spend the same installation's hour.
- A new read added beside an existing one does not copy the existing read's call shape; it reuses
  the existing read's prelude and its hold. In `packages/daemon/internal/daemon/review_permission.go`
  that is `reviewedRepository` (the pull-request record, then the repository), `rateLimitRefusal`
  (the standing-limit gate) and `holdIfLimited` (the hold on any `intake.RetryLater` the read
  returns), and both `reviewerCanWrite` and `reviewBody` go through all three.
- The test that pins a rate-limited read asserts the hold, not just the error: after the
  rate-limited answer `rateLimitLeft() > 0`, and a second read inside the window makes no GitHub
  call.

## Evidence

The first `reviewBody` copied `reviewerCanWrite`'s mint and its `RetryLater` shape and returned
the rate-limited answer without `holdRateLimit`; the reviewer proved it at 90ca0d9c with an
overlay test (two GitHub calls across two reads inside the window) and the simplify pass before
it had extracted the mint only. Fixed in c03a22fbad30 (`TestReviewBodyRateLimitedReadRetriesLater`
now asserts the hold and one call across two reads); PR #1878 thread
https://github.com/sjawhar/legion/pull/1878#discussion_r4236735153.
