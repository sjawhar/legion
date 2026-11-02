---
title: "A resumed attempt keeps the resolution its commit decided on, so record that decision in the committed event"
category: envoy
tags:
  - at-least-once
  - delivery-resume
  - idempotency-key
  - mentions
  - fan-out
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2387"
---

# A resumed attempt keeps the resolution its commit decided on, so record that decision in the committed event

Extends docs/solutions/envoy/a-fan-out-copy-defers-to-a-direct-send-only-once-that-send-was-recorded-delivered-and-the-wait-is-bounded-by-the-senders-own-lease-pr2225.md.

That note is the fan-out side: skip a recipient only once the direct send was recorded delivered.
This is the sender side.

- When a write resolves its direct recipients before it commits and another channel's fan-out is
  decided on that resolution, write the fact that the resolution decided something into the
  committed event (a boolean on its payload).
- Every later sender of that attempt - a sweep after a crash, a person's Retry of the stranded
  attempt - reads the fact back and settles the attempt with the resolution recorded at commit,
  never resolving again. Resolving again can reach a recipient the other channel already delivered
  to, under a different idempotency key, so no receiver-side deduplication catches the second copy.
- Only an attempt whose write decided nothing may resolve afresh, and so may a later attempt a
  person explicitly asked for; say so where the guarantee is documented, since it is attempt 1's.
- Read the fact both as the typed payload (the creating request holds it) and from the stored map
  (every later sender reads the event back), and let its absence mean false, so events an earlier
  release wrote behave as before.
- Prove it with a test that strands attempt 1 between its commit and its settle, then runs the
  resume path and the Retry, and counts the recipient's copies across both channels.

## Evidence

A comment that also reaches sessions by its route resolves its mentions through a listener
take-over before it commits (`packages/envoy/internal/dispatch/api/comment_create.go`), and records
`mentions_decide_routes` on `comment.created`. A resumed or retried attempt 1 first resolved the
mention afresh, pinned the now-live session and sent under `<comment>:<target>:<mode>`, which is not
the route's key, so the route holder heard the comment twice. `deliverResolvedCommentMention` now
keeps a decided attempt's recorded resolution (`decided`, `mentionsDecideRoutes` in
`api/comment_delivery.go`). `TestAResumedMentionOfARoutedCommentReachesTheRouteHolderOnce`
(`api/comment_mention_takeover_test.go`) failed first with `session s1 received the comment 2 times,
want once: 1 mention steer(s) and 1 route fan-out(s)`. Pull request sjawhar/legion#2387.
