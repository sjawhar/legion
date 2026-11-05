---
title: "A row that starts a product background loop ends it in a finally, or it fails a later row"
category: testing
tags:
  - test-isolation
  - bun-test
  - background-loop
  - shared-mock-state
  - teardown
  - flaky-tests
date: 2026-11-03
status: active
module: packages/pi-envoy
related_issues:
  - "sjawhar/legion#2699"
symptoms:
  - "a row passes alone and fails when it runs after its neighbours"
  - "a count of calls against a shared mock is one higher than the row made"
  - "a suite is green in file order and red under `-t` with a subset"
  - "a row's own timings decide another row's assertions"
---

# A row that starts a product background loop ends it in a finally, or it fails a later row

- End, in a `finally`, every session, consumer, pump or retry loop a row started. The `finally`,
  not the end of the `try`: a row whose assertion fails must still end what it started, or one red
  row leaves the leak behind for every row after it.
- Reset in `afterEach` every environment knob a row set. A runner that stops a row at its own
  timeout never runs that row's `finally`, so the knob would decide a later row's timings.
- Read a count against shared mock state as the whole suite's, not the row's. When a row counts
  calls exactly, the defect may be a row upstream that left a loop running.
- The row that needs a long interval to control its own timing is exactly the row whose loop wakes
  inside someone else's. Prefer a short interval and a structural assertion; where a long one is
  needed, that row owes its end most of all.
- Keep the proof that the loop stops: a row that starts the loop, ends the session and finds no
  further call costs milliseconds on a short interval and fails when the loop's exit conditions are
  removed. A throwaway probe that sleeps out a 60 s interval proves the same thing and belongs in
  the pull request's evidence, not in the suite.

## Evidence

sjawhar/legion#2699 (INFRA-436) added rows to `packages/pi-envoy/extensions/envoy.test.ts` that set
`ENVOY_RESUBSCRIBE_DELAY_MS` to 60000 and never ended their session. The extension's background
confirmation slept that long on a mock connection `afterEach` never closes
(`isClosed` reads `closed || natsState.connectionsClosed`), woke inside whichever row was running a
minute later, and called `flush()`, which five rows count exactly.

Measured: the reviewer's three-row selection failed 4 of 5 runs at `Expected: 1, Received: 2`; a
probe placed after the leaking row — clear the recorded flushes, sleep 61 s, expect none — received
4 stray entries. With each row ending its session the probe passed and the file was 1001 pass /
6 skip / 0 fail. The leak also cost wall time: `envoy.test.ts` fell from 115.6 s to 59.6 s once the
sessions ended, because a left-open session's reconcile pass and confirmation sit out their own
deadlines inside later rows.

It took two review rounds (`d3b573f8f26d`, `2b34e401ef63`) because the first fix ended the two rows
the finding named rather than every row the pull request added; the third leaking row was found by
running the neighbours together.
