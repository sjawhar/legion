---
title: "nats.js 2.29.3 never finishes a stale close behind a dead path and leaks an abandoned dial: patch both"
category: envoy
tags:
  - nats
  - nats-js
  - reconnect
  - ping
  - bun-patch
date: 2026-10-28
status: active
module: packages/envoy-client
related_issues:
  - "sjawhar/legion#2331"
---

# nats.js 2.29.3 never finishes a stale close behind a dead path and leaks an abandoned dial: patch both

- A client that must leave a frozen NATS path (packets stop, neither side closes) needs short pings
  and two fixes to nats.js 2.29.3, the last 2.x release. Short pings alone are not enough.
- **Size the bound from the ping settings.** nats.js drops a connection on the ping tick that finds
  `maxPingOut` pings unanswered, so a freeze is noticed within (`maxPingOut` + 1) × `pingInterval`.
  Each redial that also hangs adds the 20 s dial timeout and `reconnectTimeWait`. With 10 s and 2,
  the path is left within 30 s, or 52 s after one hung redial. At 15 s one hung redial gives 67 s.
- **Make the stale close skip the flush.** The heartbeat's stale path calls
  `ProtocolHandler.disconnect()`, which calls `transport.disconnect()`, which first flushes. A dead
  path never acknowledges what is queued, so once more is queued than the kernel buffers take, the
  flush never completes. Then `closed()` never resolves and nothing redials. Call
  `transport.close()` there. Leave `reconnect()`, the client's own reconnect on a live connection,
  on `disconnect()`.
- **Destroy an abandoned dial's socket.** When a redial times out, `NodeTransport._closed` returns
  early for a transport that never connected, so its socket stays open. A process that should exit
  on SIGTERM then keeps running. Keep the socket from the moment `dial()` creates it, and destroy it
  in `_closed` when the transport never connected. Do not remove its listeners first: the pending
  `dial`/`peekInfo` promises must still reject.
- Every session plugin dials with `@legion/envoy-client/nats-connection` (`natsConnectionOptions`).
  Put a new ping or reconnect setting there, never inline at a connect site.

## Evidence

- `patches/nats@2.29.3.patch`: one `protocol.js` hunk (`disconnect()`) and two `node_transport.js`
  hunks (`dial()`, `_closed`). Root `package.json` `patchedDependencies` lists it.
- `packages/envoy-client/src/nats-connection.ts` gives the 30 s / 52 s arithmetic.
- Locks, in `packages/envoy-client/src/__tests__/nats-stall.test.ts`:
  - Row C writes 32 MiB after the freeze. At c2db0543, with the `protocol.js` hunk reverted, it
    printed `reconnect none` and failed at 60 s. With the hunk it passed at 30 s.
  - Row B's hung redial printed `still open` without the `node_transport.js` hunks (0504cbfb).
  - claude-envoy's `tests/envoy-channel-stall.test.ts` row "still exits on SIGTERM" stayed running
    15 s after SIGTERM before the patch.
- The stale-close defect came in review round 1 (thread PRRT_kwDORFy7ds6wkMTK), after the first
  proof had passed on a proxy that drained the backlog (see
  `docs/solutions/testing/freeze-a-network-path-by-pausing-both-sockets-and-size-a-backlog-past-the-runtime-buffers-pr2331.md`).
