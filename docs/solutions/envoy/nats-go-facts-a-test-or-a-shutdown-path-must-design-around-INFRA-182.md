---
title: "nats.go facts a test or a shutdown path must design around"
category: envoy
tags:
  - nats
  - nats-go
  - jetstream
  - reconnect
  - kv-watcher
  - credentials
  - shutdown
  - logging
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-182"
  - "sjawhar/legion#2170"
---

# nats.go facts a test or a shutdown path must design around

All of these hold for nats.go v1.50.0. Before you set a test's wait or choose how it injects a
fault, measure the library's real detection time on a real server, and set the budget as a
multiple of the worst value you saw.

## Faults and how fast nats.go notices them

- **`ReconnectErrCB` runs only for a reconnect attempt whose dial failed** (nats.go:3234-3259). An
  attempt that connects and then fails the handshake writes nothing. To make a connected client's
  reconnects fail, close the address so nothing listens there. A relay that accepts a connection
  and then closes it is not enough.
- **A TCP relay whose listener closes keeps the connections already joined through it.** It goes
  on copying them, so the client never disconnects. Stop the server as well
  (`testnats.Gate.Close()`, then `testnats.Stop`).
- **Pick the gate's port outside the kernel's ephemeral range.** Docker publishes a container's
  random host port in that range, so another container could take the gate's address while it is
  closed (`testnats.StartGate` picks from 20000-29999).
- **An ordered KV consumer reports its stream gone only after two missed 5 s idle heartbeats.** It
  was measured at about 10 s, and at 18.81 s twice, against nats-server v2.11.4. A 5 s
  `eventually` can never see a `kvwatch` watcher stop, so give that wait its own budget
  (`stopNoticeBudget`, 60 s).

## JetStream calls and cancellation

- **Pass `nats.Context(ctx)` to every JetStream call that a cancellation or a deadline must
  bound.** Without it, the call runs on the handle's `MaxWait` (10 s in `internal/bus`), and
  cancelling the caller's context does nothing.
  - When ctx has no deadline, nats.go adds the handle's wait itself (jsm.go:1791-1796).
  - To keep a loop inside its own deadline, give each read
    `context.WithTimeout(ctx, deadline-waited)`.
  - A cancelled call returns `context.Canceled` or `context.DeadlineExceeded` unchanged. Neither
    matches `nats.ErrConsumerNotFound`.
- **Once a shutdown can cut the work, the error path must recognise the cut.** Making a rebuild
  cancellable made an ordered SIGTERM log
  `ERROR self-health rebuild failed ... context canceled`. Process tests fail on any ERROR line. A
  loop that runs under the shutdown's context checks `ctx.Err()` before it logs a failure.

## URL userinfo is a credential

nats.go authenticates with a server URL's userinfo (connectProto, nats.go:2807-2817). A user name
given alone is sent as the token, and `user:password` as a password. So:

- **`url.URL.Redacted()` and `nats.Conn.ConnectedUrlRedacted()` are not enough.** Both keep a user
  name given alone, which is the whole token.
- **A server nats.go discovers copies the configured URL's userinfo** (nats.go:2025-2027). So
  `nc.ConnectedUrl()` carries the credential even when the URL it names was never configured.
- **Remove all userinfo before a URL reaches a log line or a logged error.** `internal/bus` does
  this with `natsURLWithoutUserinfo`. It parses the URL and sets `User = nil`. When the URL does not
  parse, it keeps only what follows the last `@`, so nothing before an `@` can leak. That fallback
  departs from the rule "compose from parsed parts" in the LEGION-791 page, and it can drop the
  host of a URL with an `@` past its authority.
- **Test fixtures with no userinfo hide this leak.** `testnats.URL` does carry `tN:tN`, so a test
  that reads a logged URL from a connection made with it does catch the leak.

## Evidence

INFRA-182 (sjawhar/legion#2170).

- **The fault shapes and the budget.** The first CI run of the new tests was red on two of these
  facts. `reconnect_err_test.go`'s throttle-reset step closed the gate alone, which left
  `connected=true` and no failure records. The three `internal/kvwatch` repair tests waited 5 s for
  a notice that takes about 10 s (round 1 of testing).
- **The credential leak.** Review round 1 found the new reconnect WARN logging the configured URL
  verbatim. The fix (`internal/bus/reach.go`, `connect.go`) also covered the `envoy nats
  reconnected` line and the remote-NATS refusal, both of which were already in released code. On
  the real listener, with `--user/--pass` and with `--auth`, the secret appeared 0 times in the log
  at the fix and twice with the old binary.
- **The context fix.** `cmd/listener/durable.go`'s `durableProbe` and `awaitDurableBound` now pass
  `nats.Context`. Against a JetStream that never answers, a 1 s rebind deadline took 10.0 s before
  the fix and 1.0 s after.
- **The shutdown ERROR.** The guard at `runSelfHealthMonitor` (`cmd/listener/main.go`) and
  `TestRunSelfHealthMonitor_ShutdownCutRebuildLogsNoError` stop it.
