---
title: "A predicate two paths feed must read the payload shape each one gives it"
category: envoy
tags:
  - go
  - json
  - dispatch
  - outbox
  - predicates
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2508"
---

# A predicate two paths feed must read the payload shape each one gives it

A pure function over an event, a record or a message is usually written where the caller holds the
value it built, and typed assertions read it. The moment a second caller reaches it after the value
has been round-tripped through JSON — a row read back from Postgres into an `any` field, a frame
decoded off the wire, a payload stored in a column — every typed assertion in it is silently false
there, and the function answers its zero value instead of failing.

- Before you call an existing predicate from a new path, read what that path's value **is** at the
  call: find where it was decoded, not where it was built. A field declared `any` and filled by
  `json.Unmarshal` holds `map[string]any`, never the struct the other caller passes.
- Make each arm read both shapes, in one helper per field, and follow whatever the file already
  does for the one field that was right (there is usually one, written by whoever met this first).
- Pin it on the path that decodes, not only on the typed one: a table test that calls the predicate
  with built values cannot see the defect, because it never produces the decoded shape.
- A reader that prefers the new answer makes the silent zero value worse than before, not better:
  the old rule was wrong but consistent, and the new one is wrong only on the path nothing tested.

## Evidence

`events.Quiet` (`packages/envoy/internal/dispatch/events/broker.go`) decides whether an event
carries nothing for a reader following its owner topic. `Broker.Notify` calls it at append time,
where the payload is `model.MessageEventPayload` or `model.AskEditEventPayload`; the event outbox
calls it at publish time from `envelope` (`internal/dispatch/outbox/publisher.go`), where the event
came from `scanPendingEvents`, which does `json.Unmarshal(payload, &item.event.Payload)` into an
`any` field and so leaves `map[string]any`.

Two of its arms asserted the typed payloads, so a **targeted message** and a **quiet `ask.edited`**
were published `quiet: false`. Because the TypeScript renderer prefers `quiet` when it is present
(`packages/envoy-client/src/delivery.ts`), both would then have been shown to every session
following the issue, where the old `notify` rule had hidden them — a regression introduced by the
fix for a different silence. `namedVersion`, in the same file, already read both shapes; that was
the tell nobody read.

The fix added `targetedMessage` and `quietAskEdit` beside it, each a `switch` over the typed value
and `map[string]any`, and pinned the defect twice: map-shaped rows in the `events` table test, and
a case of `TestEnvelopePayloadStatesWhetherTheEventIsQuiet` that appends an event and drives it
through the outbox's own scan, so the assertion reads the wire. Before the fix that case printed
`payload quiet = false for a targeted message`.

The adopted commit's own tests passed throughout: they called `Quiet` with typed payloads only.
