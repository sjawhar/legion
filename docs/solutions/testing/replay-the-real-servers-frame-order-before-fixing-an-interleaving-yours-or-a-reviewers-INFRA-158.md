---
title: "Replay the real server's frame order before fixing an interleaving, yours or a reviewer's"
category: testing
tags:
  - frame-order
  - hocuspocus
  - ygo
  - review-rounds
  - test-fakes
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-158"
  - "sjawhar/legion#2233"
---

# Replay the real server's frame order before fixing an interleaving, yours or a reviewer's

Extends docs/solutions/testing/a-fake-built-from-the-clients-wire-shape-assumption-mirrors-the-clients-bug-a-contract-test-against-the-real-handler-catches-it.md.

That document covers a fake's response *shape*. The same holds for message *order*:

- Before you fix a race between two server frames, read the server's source and confirm it can send
  them in that order. This applies when a reviewer reports the interleaving too: check it against the
  server before you build on it, and answer the thread with what the source says.
- A test that scripts a server's frames replays the order the real server sends. Its helper cites the
  server source by file and the version the module pins, so an upgrade of that dependency reopens the
  question.
- To open a real window in a browser test, keep the server's order and delay one frame (hold the
  client's `Auth` frame, for one). Reordering frames tests a server that does not exist.
- A test that scripts an order the server never sends may stay, as a guard of a property that holds in
  any order. Its comment says the real server never sends it, so no later fix is built on it.

For Dispatch's document rooms the order is fixed. ygo writes SyncStep1 and SyncStep2 for a new socket
before its read loop starts, and writes `Authenticated` only from that loop, once it has read the
client's Auth. So a socket receives SyncStep1, SyncStep2 and awareness before `Authenticated`
(`provider/websocket/server.go` and `peer.go` of `github.com/sjawhar/ygo`, at the version
`packages/envoy/go.mod` pins).

## Evidence

- Review round 5 of sjawhar/legion#2233 reported text typed between a writable reconnect's
  authentication and its sync. Commit 52159aaf fixed that gap, and it stored the read-only load's
  heading-id stamp when the admission turned writable. ygo never opens that gap. In its real order
  the stored stamp was written after the client's SyncStep2, so no acknowledgement cleared it, and the
  false unsent-edit notice of round 1 came back. Commit 61b165e7 removed the change again.
- `packages/dispatch/web/src/features/doc/connection.test.ts`, `serveInYgoOrder`, replays SyncStep1,
  SyncStep2, awareness, then `Authenticated`. Its `beforeAuthentication` hook is the in-tree way to
  act in the real window. The tester's browser probe held only the reconnect's `Auth` frame for 3 s.
- `connection.test.ts`, the case commented "A server that authenticates the reconnect before its
  SyncStep2 arrives", still scripts the order ygo never sends.
