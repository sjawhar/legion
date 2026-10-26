---
title: "An address that stands for 'cannot reach' must refuse, and no shared fixture may reach every port"
category: testing
tags:
  - negative-control
  - real-binary-proof
  - addressing
  - test-fixture
  - envoy
date: 2026-10-26
status: active
module: packages/envoy
related_issues:
  - "INFRA-122"
  - "sjawhar/legion#2199"
---

# An address that stands for "cannot reach" must refuse, and no shared fixture may reach every port

When the defect is "dialled the wrong address", the test's wrong addresses must fail:

- In one pod every process shares loopback, so `127.0.0.1` reaches every test's port, from every
  listener. Stand in for an address that cannot reach the port with `127.0.0.2`, against a port
  bound on `127.0.0.1` only. Stand in for an address reached only one way with a name that
  resolves nowhere. An Envoy machine id cannot hold `.`, so use a single label such as
  `envoy-moved-owner-nowhere`, and check that `getent hosts <name>` fails.
- Make the rig prove the stand-in before each run: a connect to `127.0.0.2:<port>` must be
  refused. Record which address answered (the serving port's `Host` header).
- A fixture that every test shares (`liveLaneEnv`, which sets `ENVOY_HOST_BRIDGE=127.0.0.1`) must
  not set the very address under test. Build that part of the environment per cell.
- Take each count of received messages *before* the send that should cause the next one. A
  delivery that lands within milliseconds of the send arrives before a count taken after the send
  returns, and is lost to the check.
- Build the negative control as the same head with only the rule replaced. Envoy's
  `cmdtest.Build` passes `ENVOY_TEST_GO_OVERLAY` to `go build -overlay`, so a copy of
  `session.go` holding the previous round's rule builds the binary with nothing in the tree edited.
  The control then changes one variable.

## Evidence

sjawhar/legion#2199, the listener's address rule for a serving-port session:

- Round 4's tester run passed although the owner dialled its own bridge for a session on another
  machine: that bridge was `127.0.0.1`, which reached the port anyway. The round-3 reviewer found
  the defect only by re-running the re-home test through a go overlay with the owner on
  `ENVOY_HOST_BRIDGE=127.0.0.2`: no prompt in 10 s, then
  `listener delivery failed … Post http://127.0.0.2:<port>/…: connection refused`.
- Generation 3's `TestAMovedSessionReceivesEachKindOnceWhateverTheBridges` builds each cell's
  environment, with `127.0.0.2` and a name that resolves nowhere. Overlays of round 4's and round
  5's `hostFor` fail exactly cells 2 and 4, then exactly cell 5. The tester's rig checked
  `ConnectionRefusedError` on `127.0.0.2` before each run and logged the `Host` header per cell.
- The implementer's first smoke script counted prompts after the send returned and missed an
  inbox prompt delivered 4 ms after the send.
