---
title: "A reconnect handshake the server closes reaches only AsyncErrorCB"
category: envoy
tags:
  - nats
  - nats-go
  - reconnect
  - logging
  - flaky-tests
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2446"
---

# A reconnect handshake the server closes reaches only AsyncErrorCB

Extends docs/solutions/envoy/nats-go-facts-a-test-or-a-shutdown-path-must-design-around-INFRA-182.md.

- In nats.go v1.50.0, when a server accepts a reconnect's dial and then closes the connection
  during the CONNECT/PING handshake (a server that is restarting), `sendConnect` gives the read's
  `io.EOF` to `AsyncErrorCB` with no subscription. The reconnect loop then tries again without
  calling `ReconnectErrCB`.
- So an `AsyncErrorCB` that logs every error at ERROR writes an ERROR line for an ordinary NATS
  restart. Any test that asserts "no ERROR line across a restart" then flakes, and the flake is
  more likely with each extra connection the process holds.
- Classify that report in `AsyncErrorCB` as a failed reconnect attempt: no subscription,
  `errors.Is(err, io.EOF)`, and the connection's status `CONNECTING` or `RECONNECTING` when the
  callback runs. Log it as the reconnect-failure line already is, on the same throttle. Keep every
  other async error at ERROR, including an `EOF` on a connection that is connected.
- To prove it, run a real nats.go reconnect against a fake server: one that sends `INFO`, completes
  the first handshake, and on every later connection reads `CONNECT` and closes. Assert both
  halves, the reconnect `EOF` at WARN and some other async error still at ERROR. A synthetic call
  to the callback cannot show the connection's status.

## Evidence

- `TestListenerFollowsTheInterestRegistryAcrossNATSRestarts` failed once under `-race` at
  sjawhar/legion#2446's merge commit `19854b54`. It found
  `ERROR envoy nats async error name=envoy-role-lane error=EOF`, from the role-lane connection that
  pull request added. The failure did not come back in 25 `-race` runs.
- The source read: nats.go v1.50.0 `nats.go:2930-2938` (`sendConnect` pushes the error to the
  async callback) and `:3251-3258` (the reconnect loop continues without `ReconnectErrCB`).
- The fix is `1050502b`: `handshaking` and the guard in `packages/envoy/internal/bus/connect.go`
  (`:212`, `:310`). `TestAHandshakeTheServerClosedIsAFailedReconnectAttempt`
  (`internal/bus/async_error_test.go`) fails on the earlier `connect.go`, which logged
  `ERROR envoy nats async error`.
- What stays open: no test fails if the `sub == nil` guard is removed or the status check is
  widened (review round 10, `727ea565`).
