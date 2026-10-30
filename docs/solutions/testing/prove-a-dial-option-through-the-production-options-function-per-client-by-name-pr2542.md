---
title: "Prove a connection setting through the production options function, per client, by name"
category: testing
tags:
  - nats
  - test-design
  - negative-control
  - composition-root
date: 2026-10-30
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "sjawhar/legion#2542"
---

A test that dials with the option itself proves the library, not the program. It passes at the base,
where no call site carries the option, so it is no pin at all.

## Rules

- **Drive the production construction.** The listener's test starts the real `envoy-listener`
  binary, so the clients come from `listenerStreamOptions` (`cmd/listener/main.go:608-614`) and
  `openLiveTopics` (`cmd/listener/live_topics.go:35`). The daemon's test calls `natsauth.Connect`
  (`packages/daemon/internal/natsauth/natsauth.go:150-199`), the module's sole dialer.
- **Assert per constructed client, by the name it carries.** One process builds several clients from
  different call sites: the listener's main client owns the stream, its live-topics lane is a second
  client of its own. Collect the names that reported, and fail naming the ones missing
  (`cmd/listener/silent_cut_test.go:68-76`), or a change that gives the option to one sibling and
  not the other passes.
- **The negative control is removing the option from one call site**, not from the test. Measured at
  the merge head: `bus.WithSilentCutDetection()` taken out of `openLiveTopics` alone →
  `no disconnect line for envoy-live-topics 5.011s into a silent cut`, with `envoy`'s line present.
  Taken out of the daemon's `Connect` →
  `the connection was not reported lost within 5.010739211s of the server going quiet: IsConnected = true, the warning logged = false`.
- The test says so in its own words, so the next reader does not weaken it:
  `silent_cut_test.go:37-40` records that a test which connected with the option itself would report
  the cut whether or not the two call sites carry it.

## Coverage

Extends `docs/solutions/testing/a-test-only-knob-widens-the-value-production-runs-pin-it-at-the-construction-a-deployment-reaches-INFRA-56.md`
("the pin calls the composition root, never a struct literal that looks like it"). For a **dial
option** the composition root is the options function, and the pin must be per constructed client,
since one process reaches NATS from more than one call site.
