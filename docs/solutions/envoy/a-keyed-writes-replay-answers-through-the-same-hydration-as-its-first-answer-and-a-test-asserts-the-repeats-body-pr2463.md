---
title: "A keyed write's replay answers through the same hydration as its first answer, and a test asserts the repeat's body"
category: envoy
tags:
  - dispatch
  - idempotency-key
  - replay
  - hydration
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/api
---

# A keyed write's replay answers through the same hydration as its first answer, and a test asserts the repeat's body

When a field of a route's answer comes from something other than the row scan (a hydration query, a side-table load), every path that can answer that route applies it: the first run, the replay of a repeated `Idempotency-Key`, and the event payload. A replay that re-reads the row and answers it is a separate path, and a hydration added to the first run does not reach it.

- Before you add a hydrated field, list every function that builds the route's answer, the replay's recovery read included, and give each one a row in the pull request's census.
- A keyed write is not tested until a test sends the same key twice and asserts the repeat's body (`Idempotency-Replayed: ?1`) carries the field, on every route that shares the recovery read.

## Evidence

In sjawhar/legion#2463 a reply naming a block answered `reply_block` on its first send, but its repeat answered `reply_block: null`. `recoveredMessage` (`packages/envoy/internal/dispatch/api/messages.go:116-129`) serves both `replayIssueMessage` and `replayAgentMessage`. It read the message with `loadMessage`, which scans the row alone, while about ten other answer sites called `hydrateMessageReplyBlocks`. Commit `8c404ede` added the call there. `api/message_reply_block_repeat_test.go` sends each message route twice under one key and fails without the call. On the harness, a keyed reply sent twice answered `b2` both times, and the same key with another body answered `422 KEY_REUSED`.

The key layer itself (`request_keys.go`, replay, `422 KEY_REUSED`) is described in `docs/solutions/dispatch-web/a-never-ending-http-request-from-a-browser-page-pins-chromes-six-connections-once-its-cert-verifier-updates-INFRA-258.md`; this document is about the shape of the replayed answer.
