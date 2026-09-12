---
title: "Socket tests observe the peer, not the clock: half-dead sockets, microtask hops, and a fake-timer seam"
category: testing
tags:
  - bun-test
  - bun-socket
  - tcp
  - fake-timers
  - microtasks
  - worker-shim
  - worker-stream-listener
  - flake
date: 2026-09-12
status: active
module: packages/daemon/src/cli/__tests__/worker-shim.test.ts
related_issues:
  - "LEGION-22"
  - "sjawhar/legion#967"
symptoms:
  - "frames written right after the peer's listener.stop() vanish: neither the old socket nor the reconnect delivers them"
  - "expect(registrations.has(token)).toBe(false) right after `await client.closed` sees the entry still present"
  - "a listener 'timeout' test hangs for the real 5 s instead of failing fast"
---

# Socket tests observe the peer, not the clock

Three test failures during LEGION-22 (`worker-shim.test.ts`, `worker-stream-listener.test.ts`) were
not bugs in the code under test but in what the test assumed about TCP and the event loop. Each has a
reusable rule.

## 1. A socket whose peer just closed accepts writes and loses them — wait for the peer's own signal

The kill/restart test stopped the fake daemon (`server.stop(true)`), then immediately emitted frames
4–6 from the fake OMP, expecting them to land in the shim's backlog and replay after reconnect. They
vanished. The shim had not yet observed the FIN: its `Bun.Socket` still looked open, `write()` returned
the byte count, the kernel accepted the bytes into a socket whose peer was gone. No application-level
ack exists to recover them, and the spec promises only frames produced *during the gap* — i.e. after
the shim knows it is disconnected.

Rule: after killing a peer, do not emit until the component under test has **observably** reacted.
The test now injects `log` and waits for the shim's own `unavailable … retrying in 200ms` line (the
immediate redial was refused) before emitting, then waits for the `retrying in 400ms` line before
restarting the fake daemon — which also proves the real 200 ms backoff ran against a real socket. This
replaced a `Bun.sleep(250)` that guessed at both.

## 2. `.catch().finally()` on a shared promise trails a caller's own `await` by two microtask hops

The listener removed a closed stream from `registrations` via
`client.closed.catch(() => undefined).finally(unregister)`. The test did `shim.end(); await
client.closed; expect(registrations.has(token)).toBe(false)` and saw `true`. Each chained combinator
is a microtask hop: `await client.closed` in the test resumes after one hop; `.catch` runs after one,
`.finally` after a second — so the test's continuation runs *before* the cleanup. Chain directly at
registration time — `client.closed.then(unregister, unregister)` — and it is registered before any
caller can await the promise, so it always runs ahead of later continuations. General form: cleanup
that must be visible to anyone awaiting a promise must be attached with a single `.then(onOk, onErr)`
**before** that promise is handed out.

## 3. Real socket I/O cannot be driven by fake timers — inject the clock at the seam instead

`vi.useFakeTimers()` does not advance `Bun.connect`/`Bun.listen`. The listener therefore takes
`setTimeout`/`clearTimeout` as options (the daemon passes `deps.setTimeout`/`deps.clearTimeout`), and
the tests install a recording clock: every armed timer is captured (never fires on its own), every
clear is recorded. A "timeout" test then fires `timers[0].callback()` by hand and asserts the exact
`delayMs`, the single log line, and that the timer was cleared when the hello arrived first. The
listener test's `fakeClock()` helper is the shape to copy. The order of `cleared` is itself an
assertion (`[helloDeadline, waiterTimer]`) — it pins *which* code path cleared *which* timer.

Where a real wait is unavoidable — a negative check like "no log line after the peer closes" over a
real socket — keep it tiny, comment why deterministic control will not work, and never use it as the
bound for a positive condition. For positive conditions poll the observable itself: the shared
`waitFor(predicate, timeoutMs)` in `daemon/__tests__/ci-fixtures.ts` (10 ms tick; pass 10 s for a
fresh `bun` CLI child's boot).

## 4. Force the split at the handler boundary

To prove a decoder handles a UTF-8 character split across reads, do not write two halves to a real
socket — the kernel may coalesce them into one read and the test passes for the wrong reason. Build a
minimal fake `WorkerRpcSocket` (`data: { handlers: undefined }`, a `write` that records, an `end`),
attach the client, and call `socket.data.handlers.data(frame.subarray(0, k))` then the remainder with
`k` inside the multi-byte sequence (`worker-rpc.test.ts` › "reassembles a multi-byte UTF-8 character…").

## 5. Pin "X only after Y" by collapsing X and Y into one read

Sequential test code never produces the same-turn overlap by accident: two `write()` calls a few
lines apart usually arrive as two reads. Both ordering bugs found in this PR (the shim spawning after
an awaited race; the listener's post-hello remainder) only show up when the frames share a read. So
the listener's first case writes `hello` and an `agent_start` in **one** `write()`, and the shim case
calls `onLine(ACK); onLine(negotiate)` with no await between. Any invariant of the form "X is handled
only after Y" across an async boundary needs at least one test that deliberately delivers X and Y in
the same tick.

## Related

- `docs/solutions/daemon/ndjson-stream-handshake-and-framing.md` — the transport behaviours these
  tests pin (ack-shared reads, hello deadline, streaming line reader).
- `docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md` — every fix above was
  proven by running the new test against the pre-fix code first (the pre-fix listener case hangs for
  its full poll bound; the pre-fix decode case shows the U+FFFD diff).
