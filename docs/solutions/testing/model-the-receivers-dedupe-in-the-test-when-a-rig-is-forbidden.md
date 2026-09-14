---
title: "When a production-like rig is forbidden, the integration test models the receiver's dedupe explicitly and runs both deploy orders; assert the scheduled pause, not that a wait happened"
category: testing
tags:
  - integration-test
  - dedupe-key
  - manual-clock
  - sleeps-observer
  - deploy-order
  - smoke-rig
  - processes.test.ts
  - fixture-wrapper
date: 2026-09-14
status: active
module: packages/daemon/src/daemon/__tests__/processes.test.ts
related_issues:
  - "LEGION-107"
  - "sjawhar/legion#1085"
  - "LEGION-101"
  - "LEGION-15"
symptoms:
  - "an acceptance line that names 'the receiver injects the message exactly once' with no receiver in the test"
  - "a fixture's observer wrapper drops a newly-added third argument, and every assertion on it reads undefined"
  - "a re-send test that would pass with the wrong pause length"
applies_when:
  - Sami's standing order (2026-09-13) forbids a smoke rig, scratch daemon, throwaway broker, or scratch tmux server for the issue
  - The change spans two deployable parts (daemon and listener/plugin) that may land in either order
  - A test asserts timer-driven retry/backoff behaviour on an injected clock
---

# Model the receiver's dedupe in the test when a rig is forbidden

## Context

LEGION-107's acceptance line 6 originally named a smoke-rig run. Sami withdrew every rig for the
issue ("Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive
actions twice now", 2026-09-13), and the spec was rewritten so line 6 is an integration test:
with the fake runtime, fake worker client, and injected clock, a stream of `delivery_failed`
exceptions for one message against an alive architect produces at most three re-publishes, each
carrying the original `dedupe_key`, then the cap line; **a receiver in the test that drops copies by
that key records exactly one injection**. The end-to-end proof is the parent issue's production
hour after deploy. What follows is how that test (P14, `processes.test.ts` "integration: bounded
re-sends for one message against an alive architect — $listener") was made to carry the line.

## The receiver is a dozen lines of `publishRole`

The plugin's dedupe (LEGION-109) is "drop a copy whose dedupe key you have already seen". The
test injects that rule as the fixture's `publishRole`:

```ts
const seen = new Set<string>();
const injections: string[] = [];
const copies: Array<{ subject: string; json: string; dedupeKey?: string }> = [];
publishRole: (subject, json, dedupeKey) => {
  copies.push({ subject, json, dedupeKey });
  const key = dedupeKey ?? `keyless-${copies.length}`;
  if (seen.has(key)) return;
  seen.add(key);
  injections.push(json);
},
```

`copies` is what the daemon put on the wire (asserted: three, each with the triggering exception's
key); `injections` is what the model would have seen (asserted: one). The assertion that the
directive JSON sent to the pane never contains `dedupeKey` sits beside it, because the two are the
whole contract: the key travels in the publish body and nowhere else.

## Run both deploy orders as `it.each` variants

The daemon, listener, and plugin deploy separately. The stream of exceptions therefore has two
shapes, and the test runs both from one body:

| variant | exception `k`'s `dedupe_key` | receiver injections |
| --- | --- | --- |
| listener honouring the publish body's `dedupe_key` (LEGION-108) | `publish.d1` for every copy | 1 |
| listener minting a fresh key per copy (pre-LEGION-108) | `publish.d1`, `publish.fresh-2`, `publish.fresh-3` | 3 — the cap is the only bound |

Modelling the stream faithfully matters: the listener mints a fresh `event_id` per publish and the
exception reports the failed copy's id, so exception `k` carries `evt-k` and, in variant 2, a fresh
key. That is what makes the ledger-key lesson observable — a per-event-id ledger passes a test
that reuses one id and fails both variants here. State a deploy-order property in the spec
("any one of the three fixes alone stops the loop") as a table of variants, then run the table.

## Assert the pause the code scheduled, not that a wait happened

The fixture's `manager()` (since LEGION-15, #1027) exposes `sleeps(ms): EventCounter`, counted the
moment `deps.sleep` is armed, and `manualSleep()` exposes `clock.pending` and `clock.fire(ms)`.
The pattern per re-send:

```ts
const armed = sleeps(pauseMs).next();          // capture BEFORE the trigger
const handled = processes.handleException(failure(copy));
await armed;                                   // the exact pause is armed
expect(copies).toHaveLength(copy - 1);         // nothing sent before it elapses
expect(clock.fire(pauseMs)).toBeTrue();
await handled;
```

with `pauseMs = RESEND_PAUSES_MS[copy - 1]` imported from the module, and `clock.pending` asserted
empty after the cap. A wrong constant, a pause skipped for the first copy, or a re-send before the
pause all fail here; a tick-budget wait would pass all three. `next()` is captured before the
trigger, per `await-the-event-not-a-tick-budget.md`. Keep architect re-sends and worker prompts out
of the same test: the worker turn-start bound is also 5 000 ms and `fire(5_000)` resolves the
oldest wait of that length.

## The fixture wrapper that dropped the new argument

`manager()` wraps the injected `publishRole` in an observer (`published(type).increment()`). After
the conflict-forced rebase onto main's version, that wrapper was
`publishRole: (topic, json) => { injected.publishRole(topic, json); … }` — it forwarded two
arguments, and every `dedupeKey` assertion would have read `undefined`. P14's per-copy key
assertion caught it on the first run. **When a dependency gains a parameter, grep the test fixture
for the wrapper that forwards it**; an observer wrapper is a second signature to update, and its
failure mode is a silently dropped argument, not a type error, because the injected function's
optional parameter accepts the shorter call.

## What this proof does not show

A `publishRole` fake shows what the daemon sends and what a correct receiver does with it. It
cannot show that the real listener stamps the body's `dedupe_key` onto the envelope, or that the
installed plugin's dedupe actually drops the copy — those are LEGION-108's Go tests and
LEGION-109's plugin tests, and, end to end, LEGION-101's production hour (`envoy_inbox` on three
busy architects, no payload twice). Say so in the PR's E2E line, cite the standing order, and name
where the remaining proof lives; do not describe the unit suite as production-like.

## Related

- `await-the-event-not-a-tick-budget.md` — the observer fixture this builds on.
- `tick-budgets-over-real-io-flake-await-the-event-the-production-path-emits.md` — why the
  fixture exists.
- `../daemon/re-send-chains-are-keyed-by-the-message-and-a-late-receipt-is-receipt-timeout-not-delivery-failed.md`
  — what the test proves.
