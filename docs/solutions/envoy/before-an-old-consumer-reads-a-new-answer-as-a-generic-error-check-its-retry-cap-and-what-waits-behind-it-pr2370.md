---
title: "Before an old consumer reads a new answer as a generic error, check its retry cap and what waits behind it"
category: envoy
tags:
  - rollout
  - release-order
  - http-status
  - daemon
date: 2026-10-29
status: active
module: packages/daemon/internal/notify
---

# Before an old consumer reads a new answer as a generic error, check its retry cap and what waits behind it

Extends docs/solutions/envoy/a-tolerance-for-a-lagging-timestamp-ships-first-inside-the-reader-and-its-doc-states-the-rule-not-the-timeline-INFRA-610.md.

- When a producer gains a new error answer, read how each consumer of an earlier release handles
  an answer it does not know. Two facts decide the release order: whether it retries that answer
  without a cap, and whether its queue holds later work behind the stuck item.
- When both hold, one message becomes a stalled consumer, so the reader that knows the new answer
  ships first. State that order, and why, in the changelog.
- Classify only the body your own producer writes. A status code alone also comes from a proxy in
  front of it; any other body with that code stays a plain, retried error.

## Evidence

- The Legion daemon's `notify.HTTPPublisher` read any non-200 as a generic error, and the outbox
  retried it without a cap, while `earlierNoticeFor`
  (`packages/daemon/internal/daemon/notice_routing.go`) held every later notice to that architect
  behind the row. The listener's new 504s and 403s would have stalled the architect, so the daemon
  takes the release before the listeners (the envoy changelog fragment
  `packages/envoy/.changes/unreleased/pr2370-Changed-20261028-101525.yaml`).
- The tester found the 403 the listener's own grant denial gives still retried without a cap
  after the rest of the change was done (round 6). The first fix matched every JSON 403; review
  narrowed it to an error that opens with the listener's `publishDeniedPrefix`
  (`packages/daemon/internal/notify/notify.go`), and `notify_test.go` keeps
  `{"error":"forbidden"}` a plain refusal.
