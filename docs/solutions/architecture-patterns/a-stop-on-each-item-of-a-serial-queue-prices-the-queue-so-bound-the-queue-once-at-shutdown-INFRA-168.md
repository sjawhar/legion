---
title: "A stop on each item of a serial queue prices the queue, so bound the queue once, at shutdown"
category: architecture-patterns
tags:
  - serial-queue
  - timeouts
  - background-writes
  - pi-envoy
date: 2026-10-30
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-168"
  - "sjawhar/legion#2161"
---

# A stop on each item of a serial queue prices the queue, so bound the queue once, at shutdown

Extends docs/solutions/architecture-patterns/a-deadline-that-abandons-without-cancelling-makes-a-serial-queue-concurrent-INFRA-246.md.

- **Give a queued background write no stop of its own.** A stop armed when an item is queued runs down while the items before it are still being answered. So it gives up on the items with the most work ahead of them, and it tells them the service did not answer when the service never got them. A stop armed per item at a fixed length fails the same way under load, because the queue's depth, not the service, decides how long an item waits.
- **Let the client's own retry window be the only budget of a write**, and make the retry safe (one `Idempotency-Key` per intent, the same across retries). Bound the whole queue once, where the process ends: one shutdown handler waits a fixed delay, then cuts what is left and records each cut item as not confirmed.
- **Never await such a write inside a host's tool handler.** Write the local entry first, start the write without awaiting it, and record a failure as a follow-up entry keyed on that entry's own id. Otherwise one tool result waits as long as every write queued before it, past the host's handler limit.
- **Have one owner for the shutdown cut,** registered at bind time whether or not the rest of the extension loaded, so the delay and the cut are defined once.

## Evidence

- At e061d9ed each decision record had a 5 s stop. Under about 22 concurrent sessions, Dispatch answered 13 of 31 records after their stop, and those decisions were missing from the day's counts (tester round 15, `.legion/INFRA-168/test.json`).
- 2d7d78a4 armed that stop when a record was queued. Review thread `PRRT_kwDORFy7ds6xz0kO` reproduced it: with a `docEdit` that takes 3 s, records 2 and 3 got "did not answer within 5000 ms" at 5,013 ms.
- 984933ef removed the per-record stop and gave the cut one owner (`record.shutdown()`, `RECORD_CUT_DELAY_MS = 1_250` at `packages/pi-envoy/extensions/askgate/decision-record.ts:84`). At that head, 22 concurrent live sessions wrote 43 of 43 records.
- c0d1d13e (thread `PRRT_kwDORFy7ds6x189z`): the unmarked write's tool result awaited its record, which waited 120 s against a Dispatch that never answered. It now writes its entry first and starts the record unawaited (`packages/pi-envoy/extensions/envoy.ts:3811-3829`). The test 'an unmarked write's tool result returns while its record still waits on Dispatch' times out on the awaiting handler.
