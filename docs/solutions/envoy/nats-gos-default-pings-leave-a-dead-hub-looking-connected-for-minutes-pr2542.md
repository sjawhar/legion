---
title: "nats.go's default pings leave a hub that stops answering looking connected for four to six minutes; set them inside the one dialer"
category: envoy
tags:
  - nats
  - ping-interval
  - half-open
  - reconnect
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2542"
---

A hub whose packets stop without closing the socket closes nothing: `IsConnected` stays true and no
reconnect is attempted until the pings run out. nats.go's defaults are one ping every two minutes
with two outstanding, so the cut is found after four to six minutes (the doc of
`silentCutPingInterval`, `internal/bus/connect.go`). Every reader waiting on that connection waits
with it.

## Rules

- **Size the bound from the ping settings** — the Go client is the same arithmetic as the
  TypeScript one in
  `docs/solutions/envoy/nats-js-2-29-3-never-finishes-a-stale-close-behind-a-dead-path-and-leaks-an-abandoned-dial-patch-both-pr2331.md`:
  the cut is found within (`MaxPingsOut` + 1) × `PingInterval`. One second with two out puts it at
  two to three seconds, which is `bus.WithSilentCutDetection` (`internal/bus/nats.go`, whose
  `nats.Option` is `detectSilentCuts` over the constants `silentCutPingInterval` and
  `silentCutMaxPingsOut`, `internal/bus/connect.go`) and, in the daemon, `detectSilentCuts` inside
  `natsauth.Connect` (`packages/daemon/internal/natsauth/natsauth.go`).
- **Set it where the module dials, not where each caller dials.** The daemon reaches NATS only
  through `natsauth.Connect`, so the fields belong inside it and its doc says every connection it
  opens pings that way; a module whose callers pass the options has to be audited caller by caller.
  Envoy's bus has many callers with different needs, so there it is an option, applied per client
  (see the testing page beside this one).
- **The cost is a redial, never a closed connection.** A PONG later than the interval leaves one
  ping outstanding and the next PONG clears the count; a hub whose answer takes more than two
  seconds is taken for a cut one and redialled. That is the trade this deployment accepted: no test
  measures the hub's PONG latency under production load, and Dispatch's own server already runs the
  same numbers.
- **Faster detection moves every hub silence onto the reconnect path.** With the cut found in
  seconds, each one now runs the KV caches' rewatch (`internal/kvwatch/kvwatch.go`), which is why
  this change had to carry the second one: a watch a rewatch had already replaced used to end at
  ERROR, which would have been a frequent page rather than a rare one. Read what a latency change
  makes hot before shipping it alone.

## Evidence

The real `envoy-listener` binary against a NATS whose relay is frozen reports the cut at 2.79 s to
2.85 s on both of its clients, where the base reports none within five seconds; the daemon's own
`Connect` against a server that answers the handshake and then nothing reports it at 3.01 s, with
`IsConnected = true` and no warning at the base.
