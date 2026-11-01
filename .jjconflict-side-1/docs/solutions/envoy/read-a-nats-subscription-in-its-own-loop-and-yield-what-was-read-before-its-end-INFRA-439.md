---
title: "Read a NATS subscription in its own loop, and yield what was read before its end"
category: envoy
tags:
  - nats
  - nats.js
  - async-iterator
  - timestamps
date: 2026-10-27
status: active
module: packages/envoy-client
related_issues:
  - "INFRA-439"
  - "sjawhar/legion#2228"
---

# Read a NATS subscription in its own loop, and yield what was read before its end

## Rule

- When a message's arrival time decides something (here, whether a live frame arrived before a
  replay cursor started), stamp it in a loop that only reads the nats.js subscription. A consumer
  that handles messages one by one and stamps each when it reaches it stamps a message late
  whenever the message before it is slow.
- When the subscription ends cleanly or throws, yield every message already read, in order, and
  only then end or throw. nats.js's own `QueuedIteratorImpl.iterate` yields the batch it took
  before it honours `done`; a wrapper that checks "stopped" before draining drops them.
- When the caller stops reading early, call the source iterator's `return()`. nats.js acts on it
  only once the read in flight settles.
- Test a wrapper like this with nats.js's real `QueuedIteratorImpl`
  (`nats/lib/nats-base-client/queued_iterator`), not a hand-written async generator, which encodes
  whatever you assumed the library does.

## Evidence

- `readArrivals` in `packages/envoy-client/src/delivery.ts` (sjawhar/legion#2228). 564a851c
  stamps at arrival; that review thread was reopened twice before it held.
- Review round 3 found the clean-end drop. With nats 2.29.3's iterator, push 1, 2, 3, make the
  delivery of 1 slow, then stop: the raw iterator yields `[1,2,3]`, and the wrapper yielded `[1]`.
- c1c1bbf8 drains first. `src/__tests__/delivery-arrivals.test.ts` drives the real iterator, and
  all three of its tests fail against the old wrapper.
