---
title: "NDJSON socket streams: a handshake needs a positive ack, a deadline on silence, and one streaming line reader"
category: daemon
tags:
  - worker-shim
  - worker-rpc
  - worker-stream-listener
  - bun-socket
  - ndjson
  - textdecoder
  - handshake
  - backpressure
date: 2026-09-12
status: active
module: packages/daemon/src/daemon/worker-stream-listener.ts
related_issues:
  - "LEGION-22"
  - "sjawhar/legion#967"
symptoms:
  - "the daemon's first RPC frame reaches the shim before any OMP child exists to receive it"
  - "an idle pre-hello TCP connection holds its fd and buffer forever; a byte bound never fires on silence"
  - "a multi-byte UTF-8 character split across two socket reads decodes as U+FFFD in an otherwise valid frame"
  - "a Bun listener needs per-connection behaviour but Bun.listen binds one handler table"
---

# NDJSON socket streams: handshake, deadline, framing

LEGION-22 added a reverse-dialed transport for phase workers: `legion worker-shim --connect` dials the
daemon's `WorkerStreamListener`, sends `{"type":"hello","bootToken"}`, gets `{"type":"hello_ack"}`, and
the socket becomes an ordinary `WorkerRpcClient`. Four things about newline-delimited JSON over a Bun
socket were not obvious going in and generalize to any similar stream in this repo.

## 1. A handshake needs a positive ack, and the ack's read may carry the first real frame

A TCP connect succeeding says nothing about whether the peer accepted you; a rejected hello is only
observable as the connection closing some milliseconds later. Without a positive signal the shim
cannot know *when* to spawn OMP and would race the rejection. So the listener writes exactly one
`hello_ack` line before any RPC frame, and the shim spawns only on that ack.

The subtle part: the daemon's **first RPC frame can share a TCP read with the ack**. The listener
acks, registers the client, and settles `awaitRegistration` waiters in one synchronous turn; a waiter
that immediately calls `negotiate()` writes its frame into the same kernel buffer, so the shim's
reader delivers `hello_ack` and `negotiate_protocol` in one `data` event — two `onLine` calls back to
back, synchronously. The plan had the shim spawn OMP *after* `await Promise.race([acked, exit])`; by
then the second `onLine` had already run and found no child. The fix is to spawn synchronously inside
the ack callback itself (`cmdWorkerShimConnect`'s `onLine`), so the very next line already has a
stdin to go to. `worker-shim.test.ts` › "…including one sharing the ack's read" pins this by calling
`onLine(ACK); onLine(negotiate)` with no await between them.

Corollary for the listener side: bytes that followed the hello in the same chunk must be fed to the
freshly attached client (`register` → `socket.data.handlers.data(remainder)`) — a `hello` and an
`agent_start` can arrive in one write too.

## 2. A byte bound never fires on silence — arm a deadline in `open`

`MAX_HELLO_BYTES` protects against a peer that streams garbage without a newline, but a peer that
sends nothing (or drips a few bytes) holds its fd, its pending buffer, and the reader closure forever.
The reviewer's probe: an idle connection was still open 1.5 s after connect, and `helloReader`'s
`close`/`error` were no-ops. Every pre-authentication phase needs **both** bounds. The listener now
arms a per-connection timer in `open` through the same injectable `setTimeout` the `awaitRegistration`
seam already had, reusing `options.rpcTimeoutMs` rather than inventing a config key, and clears it on
every way out (register, every rejection, the peer's own close/error). Wire the daemon's `deps.setTimeout`
/`deps.clearTimeout` at the start site — a listener started on the real clock inside a daemon that
otherwise injects its clock is a test-isolation hole the reviewer will find.

## 3. One streaming line reader; never `toString("utf8")` per chunk

Three socket readers (rpc client, shim `--socket` listen side, shim `--connect` dial side) and the
shim's OMP-stdout pump all split the same NDJSON framing. The socket ones decoded each chunk with
`data.toString("utf8")`: a 3-byte character split across two reads becomes U+FFFD, silently, in a
frame that is otherwise valid JSON (only frames ≥ 1 MiB go through the base64 `rpc_chunk` path that
would catch it). `line-reader.ts` is now the read-side twin of `socket-writer.ts`: a
`TextDecoder` with `{ stream: true }`, trim-and-skip-empty, `reset()` on close so one connection's tail
never prefixes the next. The regression test drives the client's installed handlers directly with
`frame.subarray(0, splitInsideChar)` then the rest — a real socket may coalesce two writes into one
read and hide the bug, so the split must be forced at the handler boundary.

## 4. Bun binds one handler table per `listen`/`connect`; dispatch through `socket.data`

A per-connection client cannot bind its own handlers. The pattern that works (verified Bun 1.3.14):
every transport binds one static table (`workerRpcSocketHandlers`) that forwards each event to
`socket.data.handlers`; `createWorkerRpcClient` attaches by assigning that slot; a listener that must
read a preamble first installs its own sink in `open` and swaps afterwards. `socket.data` is assignable
inside `open` for accepted sockets and seeded via `Bun.connect({ data })` for dialed ones, and no
`data` event can interleave between `await Bun.connect()` resolving and the synchronous continuation
that installs the handlers.

Also verified: a refused `Bun.connect` rejects the promise and fires `connectError`, never `open` or
`close` — so a dialer's `close` handler can safely assume `open` ran (guard it anyway; it documents the
contract).

## Known limits (recorded for LEGION-24, the Kubernetes runtime)

- A permanently rejected token (unknown, stale generation) makes the shim retry forever at the 5 s
  cap, one daemon log line per attempt; nothing in the transport bounds shim lifetime on auth failure.
  The pod/process lifecycle is the backstop.
- A half-open old stream answers `already bound to a live stream` until the daemon's TCP stack
  observes the close; there is no daemon-side heartbeat, so recovery after a network partition (as
  opposed to a clean shim exit) is bounded by OS TCP timeouts.
- The listener's bind address is the literal `127.0.0.1` in three places in `index.ts` (the
  `startWorkerStreamListener` call and two log lines) until LEGION-21's `config.bind` lands.

## Related

- `docs/solutions/testing/socket-tests-observe-the-peer-not-the-clock.md` — how the tests for this
  transport avoid wall-clock guesses and half-dead-socket flakes.
- `packages/daemon/src/daemon/AGENTS.md` › Operational invariants, the shim/listener bullet — the
  wire contract this document explains the reasons behind.
