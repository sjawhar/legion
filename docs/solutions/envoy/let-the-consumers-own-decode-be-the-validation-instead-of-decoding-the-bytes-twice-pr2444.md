---
title: "Let the consumer's own decode be the validation instead of decoding the bytes twice"
category: envoy
tags:
  - performance
  - crdt
  - ygo
  - validation
  - pprof
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# Let the consumer's own decode be the validation instead of decoding the bytes twice

- When a guard decodes bytes that its consumer decodes again, remove the guard and make the
  consumer's decode the check. The decode must keep the same limits, run before anything reads
  its result, and send its error down the guard's failure path, with the same message and status.
- After a CPU fix, profile your own head and look for one input decoded twice before you call the
  fix done.

## Evidence

- `packages/envoy/internal/dispatch/docs/websocket.go`: `LoadDoc` now returns no bytes and keeps
  the stored state on the room's load record. `onLoadDocument` decodes it once, through
  `bootstrapLoadedState`, under the server's pending cap, and fails the room from that decode.
  Before, `validateUpdate` decoded the same row first.
- A pprof of 20 loads at `4bbe8232` still showed two decodes of one stored row. After the single
  decode (`ee5938e6`), `BenchmarkRoomLoad` at 240 versions took 106.0 ms, against 213.9 ms
  before it and 545.2 ms on main.
  `TestARoomWhoseHistoryDoesNotDecodeFailsFromTheRoomsOwnDecode` proves the failure path.
