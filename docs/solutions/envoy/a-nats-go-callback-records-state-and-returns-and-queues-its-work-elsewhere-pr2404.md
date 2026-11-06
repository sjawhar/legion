---
title: "A nats.go callback records state and returns, and queues its work elsewhere"
category: envoy
tags:
  - nats
  - nats-go
  - reconnect
  - callbacks
date: 2026-10-31
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2404"
---

# A nats.go callback records state and returns, and queues its work elsewhere

## The rule

1. nats.go runs every callback of a connection one at a time, on one dispatcher goroutine, in
   event order. A callback that blocks holds every later callback of that connection behind it,
   its disconnect and close included.
2. So a connection callback does only what must keep nats.go's order and takes no time: it
   updates state (an atomic, a counter under a short lock) and returns.
3. Anything that makes requests, such as restoring subscriptions, re-watching KV buckets or
   running reconnect hooks, goes to a goroutine your client owns. Run it on one serialized worker,
   oldest event first, so the work still runs one event at a time.
4. Leave the state writes on the dispatcher. Then a set and a later clear keep nats.go's own
   order, and the worker can never undo a disconnect that came after the reconnect it is working
   through.
5. Test it: cut the link while a reconnect hook blocks, and require the client to read
   disconnected within a second.

## Evidence

- Before commit 581c5f4b, `ReconnectedCB` set the connected flag and then ran `Client.onReconnect`
  (restoring subscriptions, then the hooks) on the dispatcher. A link cut during those hooks
  queued the `DisconnectedErrCB` behind them. The review saw `Connected()` read true 12.19 s
  after a cut, past the lease's 15 s take-over bound.
- 581c5f4b keeps the flag's set and the lock-only bookkeeping (`countConnectionEvent`) on the
  dispatcher and hands the work to `queueReconnect` and `workThroughReconnects`
  (`packages/envoy/internal/bus/recovery.go`).
  `TestConnectedReadsDownAtACutWhileTheReconnectHooksRun` (`internal/bus/connected_cut_during_hooks_test.go`)
  failed first ("still reads connected 1.005s after the link was cut"); the cut is now read 6 ms
  after it.
