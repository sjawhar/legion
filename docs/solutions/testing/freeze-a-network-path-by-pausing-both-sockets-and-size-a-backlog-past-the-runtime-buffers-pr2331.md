---
title: "Freeze a network path by pausing both sockets, and size a backlog past the runtime's buffers"
category: testing
tags:
  - network-partition
  - tcp-proxy
  - backpressure
  - negative-control
  - bun
date: 2026-10-28
status: active
module: packages/envoy-client
related_issues:
  - "sjawhar/legion#2331"
---

# Freeze a network path by pausing both sockets, and size a backlog past the runtime's buffers

Extends `docs/solutions/testing/a-stopped-bus-cannot-prove-a-request-bound-only-a-connected-silent-one-can-INFRA-141.md`.
That document asks for a connected, silent peer. A silent peer that still reads is a different
fixture from a dead route.

- **To model a dead route, stop reading as well as forwarding.** Pause both sockets of every open
  connection (`socket.pause()`), close neither side, and create the server with `allowHalfOpen`.
  The client's unsent bytes then stay in its own socket, as they do behind a route that drops
  packets.
- **A proxy that reads and discards is not a dead route.** The kernel keeps acknowledging, so
  every failure that depends on back-pressure is hidden.
- **A design note that says "a proxy that stopped reading could hang the client" names a
  production bug.** Write a test for it. Do not choose the fixture that avoids it.
- **Connections opened after the freeze forward normally,** as a load balancer sends a new flow to
  a live backend. Keep a separate mode for "accepts, then never answers" (`stallNext(n)`). That
  mode reads and discards, so the client's own close of a hung redial can be seen.
- **A backlog load depends on the runtime's buffers.** A lock whose failure needs more queued data
  than the kernel and runtime take may stop failing after a runtime upgrade:
  - Size the load several times past the threshold you measured.
  - Write the threshold and the runtime version beside the constant.
  - Run the negative control again whenever the branch takes in a new `.bun-version`.

## Evidence

- `packages/envoy-client/test/stall-proxy.ts` `freeze()` pauses `client` and `target`.
  - The first version read and discarded both sides. The plan's gap analysis chose that ("The
    proxy reads and discards both sides of a frozen connection") to keep nats.js's close from
    waiting on a full buffer.
  - Review round 1 (thread PRRT_kwDORFy7ds6wkMTS) showed that this hid the stale-close hang that
    `docs/solutions/envoy/nats-js-2-29-3-never-finishes-a-stale-close-behind-a-dead-path-and-leaks-an-abandoned-dial-patch-both-pr2331.md`
    describes.
- `packages/envoy-client/src/__tests__/nats-stall.test.ts`, row C:
  - Written at 4 MiB on Bun 1.3.14, where it failed without the patch.
  - On Bun 1.4.4 4 MiB drained and the row passed without the patch. The tester raised it to
    32 MiB in a5a67218, where it fails without the patch (60 s) and passes with it (30 s).
  - The comment beside `unackedBytes` records 4 MiB drains, 8 MiB hangs, 32 MiB margin.
