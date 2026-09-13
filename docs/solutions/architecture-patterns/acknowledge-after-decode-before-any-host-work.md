---
title: "An acknowledgement that bounds a remote timeout is the first effect after the frame is provably decodable; name the failures it makes silent"
category: architecture-patterns
tags:
  - envoy
  - pi-envoy
  - core-nats
  - receipt
  - request-reply
  - timeout
  - dedupe
  - failure-visibility
date: 2026-09-14
status: active
module: pi-envoy
related_issues:
  - "LEGION-109"
  - "LEGION-101"
  - "sjawhar/legion#1083"
---

# An acknowledgement that bounds a remote timeout is the first effect after the frame is provably decodable; name the failures it makes silent

## The incident

The Envoy listener forwards a role-lane message to the holder's agent subject as a core-NATS
request and waits two seconds for a receipt; a miss is a `delivery_failed` exception, which the
Legion daemon answers by re-sending. The pi-envoy plugin's pump published that receipt as the
**last** statement of `deliver()` — after the inbox update, after any Dispatch round trip
(`postDispatchReply`, `askEphemeral`), after `pi.sendMessage`. Under load a busy session's turn
outlasted the window on every message, so every role message to a busy holder was redelivered
every few seconds (LEGION-101). The receipt was measuring the host's turn, not delivery.

## The rule

Find the **last purely local, exception-only step** in the handler — the one after which the frame
is known to be well-formed and before which no variable-latency work has started — and publish the
acknowledgement immediately after it. In `deliver()` that seam is `renderInbound()`: synchronous,
side-effect-free, tolerant (non-JSON and schema-invalid frames render as `unrecognised` rather than
throwing), so the receipt sits between it and the `subscriptionRemovedTopics` loop, under the same
guard as before (`reply !== "" && subject === agentSubject(sessionID)`):

```ts
const rendered = renderInbound(raw, sessionID, subject);
if (reply !== "" && subject === agentSubject(sessionID)) {
  try {
    (await ensureConnection()).publish(reply);
  } catch (error) {
    console.warn(`[envoy] failed to acknowledge envelope ${rendered.envelope?.event_id ?? "unknown"}`, error);
  }
}
// inbox update, dedupe check, Dispatch calls, injection follow — unchanged
```

The receipt now means "a live receiver has this frame", not "the host has finished with it".
Everything after it is host work with no latency bound, and none of it belongs inside a two-second
window.

Three consequences of the placement, each deliberate:

- **A frame the decoder throws on is still never acknowledged.** The receipt is after the decode,
  not before it, so a wire-level problem still surfaces to the listener as a timeout. Acknowledging
  before decoding was rejected for exactly this reason.
- **The receipt is best-effort; the local delivery is not.** `ensureConnection()` and `publish()`
  share one `try/catch` because their consequence is identical — no receipt reaches the listener —
  and so is the response: log with the event id, continue. A thrown receipt must not lose the
  message locally. Splitting the two failure modes adds nothing a reader of the log could act on.
- **Dedupe is untouched.** `dedupeKeys.add` still runs only after a successful injection. The
  daemon's re-send path (LEGION-107) relies on that: a copy that did arrive is acknowledged and
  dropped by the receiver; one whose injection threw is unrecorded, so a re-send injects it.

## Name what the move makes silent

Moving an acknowledgement earlier shrinks the set of failures the **sender** can observe. Before
the move, an injection that threw (mid-compaction, say) skipped the trailing publish, the listener
timed out, and the daemon re-sent. After it, that failure is acknowledged, logged locally with the
event id, and left for a later deliberate re-send or the role's catch-up — nothing external learns
of it. That is the spec's Errors row 2 and the reviewer named it as a spec-sanctioned change, but
the fresh-eyes pass on this retro found that the code docs describe only the failure that
*stopped* being reported (a busy-but-alive holder) and not the one that *became* silent. When you
move an ack, write both lists into the module's `AGENTS.md`: the false positives it removes and the
true failures it now hides, with the recovery path for each (here: receiver-side dedupe plus a
daemon re-send or catch-up).

## Where else this seam exists

Any Legion/Envoy contract keyed to a receipt, ack, or heartbeat with a remote deadline: the
listener's role lane (this case), the daemon's worker-shim RPC acknowledgement versus
turn-start observation (`PromptReceipt` in `worker-rpc.ts` already separates "acknowledged" from
"turn started"), JetStream ack patterns. Ask where the frame becomes provably decodable, put the
timing-critical signal there, and treat the rest as host work.

## Related

- [core-nats-receipt-backed-current-holder-delivery](core-nats-receipt-backed-current-holder-delivery.md)
  is the listener side of the same contract (the request, the two-second wait, the exception).
  Its snippet was updated by LEGION-109; its prose at "replies only after accepting" still
  describes the old order and is on the docs-sweep list recorded on LEGION-109.
- [prove-call-order-with-a-recorded-sequence-not-microtask-counting](../testing/prove-call-order-with-a-recorded-sequence-not-microtask-counting.md)
  is how the new order was proven and locked.
