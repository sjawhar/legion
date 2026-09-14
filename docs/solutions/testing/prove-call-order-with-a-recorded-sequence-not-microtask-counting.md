---
title: "Prove call order with one recorded sequence whose awaited point provably differs under the old order, not with microtask counting"
category: testing
tags:
  - bun-test
  - call-order
  - recorded-sequence
  - microtasks
  - fail-pre-fix
  - pi-envoy
  - mock-module
date: 2026-09-14
status: active
module: pi-envoy
related_issues:
  - "LEGION-109"
  - "sjawhar/legion#1083"
---

# Prove call order with one recorded sequence whose awaited point provably differs under the old order, not with microtask counting

## The smell

The test that guarded the receipt in pi-envoy's `deliver()` read:

```ts
await injected.promise;
await Promise.resolve();
await Promise.resolve();
expect(natsState.published.map((m) => m.subject)).toEqual(["_INBOX.receipt"]);
```

Two bare microtask ticks after the injection resolved, the receipt had been published. That
asserts *that* the publish happens, at a depth tuned to the implementation's exact `await` count;
it says nothing about *when* relative to the injection, and it breaks silently if the code gains
or loses an `await`. It could not fail for LEGION-109's bug (receipt after the host's work) because
it never looked at the order.

## The pattern

One `calls: string[]`, fed by **every** side the order is about, and one `expect(calls).toEqual([...])`:

```ts
const calls: string[] = [];
let recording = false;
globalThis.fetch = async (input, init) => {
  if (recording) calls.push(`fetch ${new URL(input.toString()).pathname}`);
  return responseWithRegistration(input, init, []);
};
envoyExtension({
  ...fixture.pi,
  sendMessage: (message, options) => {
    calls.push("sendMessage");
    fixture.pi.sendMessage(message, options);
    injected.resolve();
  },
});
await fixture.handlers.get("session_start")?.({}, sessionContext("ses_receipt"));
natsState.onPublish = (subject) => calls.push(`publish ${subject}`);
recording = true;                      // arm after setup so registration fetches are not recorded

controls.push(forwardedRoleEnvelope("legion-controller", "receipt event", "agent-receipt"), "_INBOX.receipt");
await injected.promise;
expect(calls).toEqual(["publish _INBOX.receipt", "sendMessage"]);
```

The NATS mock gained one hook and one fault:

```ts
publish: (subject, data) => {
  if (natsState.failPublishes > 0) { natsState.failPublishes -= 1; throw new Error("PUBLISH_FAILED"); }
  natsState.published.push({ subject, data });
  natsState.onPublish?.(subject);
},
// afterEach: natsState.onPublish = undefined; natsState.failPublishes = 0;
```

`onPublish` records the publish in the same log as the other sides; `failPublishes` is a one-shot
fault for the "acknowledgement itself fails" row. Both reset in `afterEach` so a later test cannot
inherit a hook or a pending throw.

## The awaited point must discriminate

Choose the promise you `await` so that, at the moment it resolves, the **old** order has provably
not yet done the thing you assert first. `injected` resolves inside `sendMessage`: under the new
order the receipt is already in `calls`; under the old order it is not (the trailing publish ran
after `sendMessage` returned). The same discipline for the other rows:

| row | awaited point | old order at that point | asserted |
| :-- | :-- | :-- | :-- |
| BTW frame | the reply POST arrives in `fetch` | receipt not yet published | `["publish _INBOX.btw", "askEphemeral", "fetch …/reply"]` |
| injection throws | a later message's `sendMessage` | `_INBOX.failed` never published (the throw skipped the trailing publish) | `["_INBOX.failed", "_INBOX.retried", "_INBOX.after"]` |
| duplicate | the third message's `sendMessage` | `_INBOX.next` not yet published | `["_INBOX.first", "_INBOX.duplicate", "_INBOX.next"]` |
| receipt publish fails | the second message's `sendMessage` | the throw escaped `deliver()` after injection: no warning, no later receipt | `["_INBOX.kept"]` and one warning naming `evt-receipt-lost` |

Record the red before touching the code. The plan predicted the duplicate row would pass on the
old code ("the old code also acknowledges duplicates, one microtask later"); it failed, because the
third receipt had not been published when the third `sendMessage` resolved the awaited promise —
the test observed the old order too. A red you did not predict is the test telling you what it
really discriminates; read it, then decide whether that is what you want it to lock. Here it was
(5 fail / 0 pass before; 5 pass / 0 fail after), and the reviewer's independent check confirmed
each test fails the old order deterministically with no sleeps and no tick counting.

## What not to do

- Do not count ticks (`await Promise.resolve()` ×N, `setTimeout(…, 0)`) to "let the other side
  run". The count encodes the implementation; the assertion then pins a depth, not an order.
- Do not assert one side's array (`published`) and infer the other side's timing from the test's
  own position in the file. Put both sides in one log.
- Do not record from before setup. `session_start` performs its own fetches; gate the log
  (`recording = true`, or arm `onPublish` after setup) so the sequence starts at the first push.

## Related

- [acknowledge-after-decode-before-any-host-work](../architecture-patterns/acknowledge-after-decode-before-any-host-work.md)
  is the production change these tests lock.
- [race-regression-tests-that-fail-before-the-fix](race-regression-tests-that-fail-before-the-fix.md)
  is the same "prove the pre-fix failure" rule for races proper.
- [bun-mock-module-global-leak](bun-mock-module-global-leak.md) — why the `nats` mock's state lives
  in one module-level object that `afterEach` resets, which is where `onPublish`/`failPublishes` went.
