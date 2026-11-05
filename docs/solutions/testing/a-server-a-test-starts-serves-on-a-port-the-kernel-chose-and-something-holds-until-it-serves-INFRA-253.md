---
title: "A server a test starts serves on a port the kernel chose and something holds until it serves"
category: testing
tags:
  - ports
  - address-already-in-use
  - test-harness
  - held-socket
  - flaky-tests
date: 2026-10-27
status: active
module: scripts/e2e
related_issues:
  - "INFRA-253"
  - "sjawhar/legion#2229"
---

# A server a test starts serves on a port the kernel chose and something holds until it serves

Extends docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md.

## Rule

- Never find a free port, close it, and start a server on that number. Between the close and the
  server's bind any program can take the port, and the server exits on `address already in use`.
- Pick the mechanism by who needs the port first:
  - Nothing needs it before the process starts: the process binds port 0 and the harness reads
    the port from the process's own listening line. `nats-server -p -1` (`-1` is nats-server's
    `RANDOM_PORT`; `-p 0` means its default 4222), envoy-listener `PORT=0`,
    `Bun.serve({ port: 0 })` then `server.port`.
  - The process's own settings must name its port (Dispatch's `DISPATCH_SERVER_URL` is its CSRF
    origin): the harness opens the socket on port 0 and hands it over as a descriptor
    (`scripts/e2e/lib/held-port.py exec` in shell, `cmdtest.HoldPort(t).Pass(cmd, ...)` in Go,
    with `{port}` substituted into the settings).
  - The port must survive restarts, or a config file names it before the process starts: a keeper
    holds one socket for the whole run and hands each start a copy (`held-port.py keep` plus
    `exec --from`, `rig.sh`'s `keep_port` and `start_kept`).
- A server's listening log line prints the bound address (`ln.Addr()`), never the configured one,
  or a port-0 server reports port 0.
- A port someone sets explicitly is used as given and fails with `address already in use` when it
  is taken. Never replace it with another.
- A held or kept socket accepts and queues connections before any server serves it, and between
  two servers on a kept port. A request to it waits instead of being refused. Give every readiness
  or liveness poll a per-request timeout, treat a timeout as "not up" (or "down"), and wait for the
  server's own listening line rather than taking an answer as readiness.
- These do not close the race, so do not reach for them: a port band below
  `ip_local_port_range` (it keeps only its own callers apart, and a pod's range can start at
  16000), a registry of ports already handed out (one binary only), a retry on
  `address already in use` (it runs the same race again), a "port taken?" probe before the start
  (the port can be taken after it).
- Prove a held port deterministically: a child that waits on stdin before it serves, a competing
  bind that must get `EADDRINUSE` in that window, and as the negative control the holder mutated to
  release early. A parallel stress run is an acceptance check, not the control: the old race is
  intermittent and can pass.
- After each forward merge of main, search the new code for the pattern again (`net.Listen` on
  `:0` then `Close`, `pick_port`, a fixed default): main can bring it back.

## Evidence

- `cmdtest.FreeTCPPort` (deleted in `packages/envoy/internal/cmdtest/cmdtest.go`) picked numbers
  in 20000-32767 and remembered what it handed out; `envoy-infra-go` still failed with
  `127.0.0.1:40077: bind: address already in use` (sjawhar/legion#2158, run 40149911299).
- `HoldPort` and its regression `TestHeldPortStaysHeldUntilItsProcessServes`
  (`packages/envoy/internal/cmdtest/cmdtest_test.go`) are the deterministic proof; with
  `HoldPort` releasing early it fails at `cmdtest_test.go:87`. The parallel acceptance run
  (4 copies x 3 rounds, run 40292180529) passed at the head and at the base alike.
- A plain `http.Get` to a held socket hung `go test` for its whole 10-minute timeout in a negative
  control of `cmd/listener`'s `waitHealthy`; it now bounds each request and waits for the
  `envoy-listener listening` line. `held-port.test.ts` flaked once by reading the listening line
  right after `/healthz` answered; it now awaits the line.
- Main's #2264 added `freeLoopbackPort` (listen, close, reuse the number) in
  `packages/envoy/cmd/dispatch/pprof_test.go` while #2229 was open; the merge replaced it with
  `startPprof(ctx, "127.0.0.1:0")`.
