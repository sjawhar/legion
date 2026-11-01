---
title: "A confirmed send keeps its own confirmation record; a cache written on a skipped push is no receipt"
category: envoy
tags:
  - delivery
  - deduplication
  - confirmation
  - listener
date: 2026-10-29
status: active
module: packages/envoy/cmd/listener
---

# A confirmed send keeps its own confirmation record; a cache written on a skipped push is no receipt

- A route that answers success only once a recipient confirmed must read a record that only its
  own confirmations write. Do not reuse a delivery cache that another path also writes.
- Say at the record's field, in one sentence, who may write it.
- Prove it with the other writer running: a test with every delivery path on, a recipient that
  never confirms, and an answer that must name it.

## Evidence

- The listener's delivery handlers record `(dedupe key, session)` in their `dedupeCache` for a
  self-subscribed session whose push they skipped (`fanoutDelivery`, `agentSubjectDelivery` in
  `packages/envoy/cmd/listener/delivery.go`), having heard nothing from it. With the live topics
  lane on (`ENVOY_SUBSCRIBE_ALL_TOPICS=1`), the publish route's own core publish reached that lane,
  so a route that read `dedupeCache` answered 200 in 0.22 s for a recipient that never confirmed
  (review of sjawhar/legion#2370, round 1).
- The fix is `listenerDeps.confirmed` (`packages/envoy/cmd/listener/main.go`), a second
  `dedupe.Cache` that only the route writes, on a receipt (`confirmed.go`).
- `TestPublishRouteDoesNotCountTheLiveLanesSkippedPushAsAConfirmation` (`confirmed_test.go`) runs
  the real live lane with a silent recipient and expects 504 `not_confirmed`. The lane-on subtest of
  `TestServerChange` (`server_change_test.go`, tag `serverchange`) loses ad-hoc messages answered
  200 with the old read.
