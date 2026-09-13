---
title: "Locking an error-to-reason mapping with an injectable forward seam and a paused NATS testcontainer"
category: testing
tags:
  - go-test
  - testcontainers
  - nats
  - fault-injection
  - error-sentinels
  - test-seams
  - structured-logs
  - regression-lock
date: 2026-09-14
status: active
module: envoy
problem_type: testing
component: cmd/listener, internal/bus
severity: high
applies_when:
  - A consumer maps a producer's errors onto a small set of outcomes (exception reasons, metric labels, log statuses) and both sides need locks
  - A fault injection you believe is fatal stays green in every committed suite
  - The behaviour under test is a network timing distinction (buffered-but-unflushed vs. flushed-and-unacknowledged) that a fake error or a fake clock cannot reproduce
  - A delivery outcome must be asserted through its log line as well as its emitted event
  - The pinned testcontainers-go has no `Pause`/`Unpause` on its container type
related_issues:
  - "LEGION-108"
  - "sjawhar/legion#1079"
---

# Locking an Error-to-Reason Mapping with an Injectable Forward Seam and a Paused NATS Testcontainer

## Context

LEGION-108 taught the Envoy listener a third role-lane exception reason: a forward that reached the
server and drew no receipt is `receipt_timeout`; a forward not known to have left the process stays
`delivery_failed`. The distinction lives at two places — `bus.Client.RequestCoreTo` decides which
error to return, `roleTopicDelivery` decides which reason each error becomes — and the round-1 review
found the producer side wrong: the flush's `nats.ErrTimeout` was indistinguishable from the receipt
wait's (see the receipt-backed delivery pattern's *Pitfalls* section). The fix added a package sentinel
(`bus.ErrReceiptTimeout`). Then the tester injected the obvious fault — return the sentinel from the
flush-error branch too — and **both committed suites stayed green**. Only a throwaway probe that paused
the NATS server turned it red. This page records the three seams and the one real-transport test that
together lock such a mapping without any rig, and the fault-injection discipline that exposed the gap.

## Guidance

### 1. Make the producer call a field, not a method (`forwardRole`)

```go
type listenerDeliveryHandlerConfig struct {
	client *bus.Client
	// forwardRole delivers a role-lane envelope to the holder's agent subject and
	// waits for its empty receipt. main.go wires client.RequestCoreTo.
	forwardRole func(subject string, item contracts.Envelope, timeout time.Duration) error
	// …
}
```

`main.go` sets `forwardRole: client.RequestCoreTo`; the test harness does the same and exposes its
`config`, so a test copies it, replaces the one field, and builds its own handler
(`coreNATSDeliveryHandler(cfg)`). Every other dependency stays real (the registry, the session
registry, the caches, the exception publisher over the real NATS connection). The consumer's mapping is
then a table with one row per error shape, deterministic and instant:

```go
{"raw nats.ErrTimeout from the flush", natsgo.ErrTimeout, "delivery_failed", "listener role forward failed"},
{"flush timeout wrapped by the client", fmt.Errorf("bus: flush forward: %w", natsgo.ErrTimeout), "delivery_failed", "listener role forward failed"},
{"bus.ErrReceiptTimeout", bus.ErrReceiptTimeout, "receipt_timeout", "listener role receipt timed out"},
{"bus.ErrReceiptTimeout wrapped", fmt.Errorf("forward: %w", bus.ErrReceiptTimeout), "receipt_timeout", "listener role receipt timed out"},
```

Include the *raw vendor sentinel* as a row that must map to the failure reason, and the wrapped forms
of both. The raw row is what fails when someone re-keys the branch on `nats.ErrTimeout`; the wrapped rows
are what fail when someone compares with `==` instead of `errors.Is`.

### 2. Give the logger a writer seam and capture it per test

```go
func New(machineID string) *Logger { return NewWithWriter(machineID, os.Stderr) }

func NewWithWriter(machineID string, w io.Writer) *Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &Logger{machineID: machineID, logger: slog.New(handler)}
}
```

The harness wires `logging.NewWithWriter("test", logs)` where `logs` is a mutex-guarded
`bytes.Buffer` (delivery handlers run on NATS callback goroutines), keeps `logs` on the harness, and
dumps it with `t.Logf` only when the test fails. Tests then assert the second channel independently of
the exception envelope — `"msg":"listener role receipt timed out"` and
`"delivery_status":"receipt_timeout"` for the timeout row, and for every `delivery_failed` row that the
string `receipt_timeout` appears nowhere in the log. The emitted reason and the logged status can drift
apart in a refactor; two channels catch that where one would not. The same lines double as the PR's
evidence: `-v` output quoting a real log line and a real exception payload is what the architect asked
for in place of a rig.

### 3. Make the exception assertion return what it decoded

`assertDeliveryException(t, probe, original, reason) contracts.Envelope` validates the exception
envelope and payload field by field and returns the envelope. Existing callers ignore the value; the
new test logs `exception.Payload`, and the re-publish test uses the returned event id. A helper that
swallows what it decoded forces every later test to re-decode.

### 4. Lock the producer against a real stalled transport: pause the testcontainer

The seam locks the *consumer*. The *producer* — "does `RequestCoreTo` return the sentinel from the
flush branch?" — cannot be locked by injecting errors past it, and a subject with no subscriber only
exercises the receipt-wait exit. Drive the flush branch for real:

```go
ctr, uri := startNATS(t)                       // the package's own nats:2.10 testcontainer
client, _ := bus.Connect([]string{uri})
// … subscribe a responder on the subject and prove one receipted round trip as the control …

docker, err := testcontainers.NewDockerClientWithOpts(ctx)
if err != nil { t.Skipf("no docker client to pause the fixture: %v", err) }
defer docker.Close()
if err := docker.ContainerPause(ctx, ctr.GetContainerID()); err != nil {
	t.Skipf("the fixture cannot be paused: %v", err)
}
// Cleanups run last-in first-out, so this unpause precedes startNATS's Terminate.
t.Cleanup(func() { _ = docker.ContainerUnpause(context.Background(), ctr.GetContainerID()) })

err = client.RequestCoreTo(subject, item, 500*time.Millisecond)
// want: err != nil; !errors.Is(err, bus.ErrReceiptTimeout); errors.Is(err, natsgo.ErrTimeout);
//       strings.HasPrefix(err.Error(), "bus: flush forward: "); elapsed within [window, 2s]
```

Why this reproduces the reviewer's scenario faithfully: a paused server keeps the TCP connection open,
so the client stays `CONNECTED` — `ensureConnWithContext` returns at once, `SubscribeSync` and
`PublishRequest` buffer without error, and `FlushTimeout` waits for a PONG that cannot arrive. That is
the reconnecting-or-stalled connection with the forward still in the buffer; no fake error and no fake
clock produces it. Notes:

- testcontainers-go v0.41.0 has no `Container.Pause`/`Unpause`. `testcontainers.NewDockerClientWithOpts`
  returns a `*DockerClient` that embeds the moby `*client.Client`, so `ContainerPause`/`ContainerUnpause`
  are reachable without adding `docker/docker` as a direct dependency.
- Keep the request window short (500 ms) and assert the elapsed time is within it and well under the
  client's default 10 s flush — the bound is part of the contract.
- Unpause in `t.Cleanup` registered *after* the fixture's own cleanup so it runs first (LIFO); a paused
  container cannot be terminated cleanly, and the fixture is shared with the rest of the package's run.
  Run the package suite twice after adding the test and confirm `docker ps --filter status=paused` is
  empty afterwards.
- Skip cleanly (`t.Skipf`) when the docker client or the pause is unavailable rather than failing the
  suite on an environment the fixture itself already needs.

## Fault-injection discipline

The gap was found because the tester ran a *named* fault (`nats.go` flush-error return replaced by
`return ErrReceiptTimeout`) against the whole package and expected exactly one lock to trip. When
every committed suite stayed green, that was the finding — not a pass. Rules that held up on this
issue:

- Every regression lock is proven by at least one fault that makes it, and only it, fail; name the
  fault and the failing assertion in the handoff (`request_core_test.go:154: a flush timeout was
  reported as the receipt sentinel`).
- Commit the GREEN state *before* injecting a fault. `jj restore <file>` reverts the whole file to the
  parent commit, so a fault injected into a file that also carries uncommitted good changes takes the
  good changes with it when restored (this bit once on LEGION-108).
- Run the fault against the whole package, not just the new test, to learn which locks cover it —
  and, when the answer is none, which surface the missing lock needs (here: the real transport).

## Related

- [Receipt-backed core NATS delivery to current holders](../architecture-patterns/core-nats-receipt-backed-current-holder-delivery.md) — the pattern these locks protect, and its *Pitfalls learned on LEGION-108* section for why the flush and receipt exits needed distinct errors.
- [Race regression tests: gate the racing side, hard-assert the precondition, prove the pre-fix failure](race-regression-tests-that-fail-before-the-fix.md) — the Bun-side twin of "a lock must fail before the fix".
- [testcontainers-go resolves all Docker credhelpers](testcontainers-go-resolves-all-docker-credhelpers.md) — the fixture these tests share, and its one known local failure mode.
