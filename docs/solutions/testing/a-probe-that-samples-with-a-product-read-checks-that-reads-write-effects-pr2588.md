---
title: "A probe that samples with a product read checks that read's write effects"
category: testing
tags:
  - e2e
  - observation
  - race
date: 2026-11-04
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2588"
---

# A probe that samples with a product read checks that read's write effects

Extends docs/solutions/testing/ask-the-thing-itself-at-the-moment-the-claim-is-about-INFRA-250.md.

That page says to ask the real object at the real moment. This one is its other half: a product
read is a product call, and some product calls write.

- **Before you poll a state with a product read, read that read's own documented write effects.**
  A lookup that repairs, releases, expires, refreshes or lazily reconciles what it reports will
  take away the state you are waiting to observe, and the probe then reports the state its own
  polling produced.
- **Where the read writes, do not poll.** Drive the window from the actor's own signal — a
  deterministic hook, a stopped clock, a fixture that holds the state — and read once, afterwards.
- **A measurement taken through such a read is discarded, not reinterpreted**, and the report says
  so: a run whose probe changed the state proves nothing about the state, and a later reader needs
  to know which run the numbers came from.
- A read whose write effect is the thing you are testing is the opposite case and is fine: then
  the read is the act, and the probe is a second observer of it.

## Evidence

sjawhar/legion#2588's tester measured how often a publish to a role whose claim had lapsed
answered `holder_lapsed` rather than `unclaimed`. The first attempt polled the role with
`GET /v1/roles/<role>` to catch the window. That lookup releases a lapsed claim as it reports it —
the handler's own doc says the 404 carries "the revision of a lapsed claim still standing, or 0
for a role no record claims — an unclaimed one, and a lapsed holder whose claim this lookup
released" (`packages/envoy/cmd/listener/api.go:1348-1352`) — so every answer read `unclaimed` and
the race the probe existed to catch never happened.

With the poll removed and the window driven by the actor itself, 3 of 12 publishes saw
`holder_lapsed` and 9 saw `unclaimed`, which is the measurement the pull request records.
