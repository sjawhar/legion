---
title: "A Custom UnmarshalJSON Is Promoted Through Struct Embedding"
category: envoy
tags:
  - envoy
  - go
  - json
  - contracts
date: 2026-09-07
status: active
module: envoy
symptoms:
  - "a response struct embedding `contracts.Envelope` decodes with its own fields always empty"
  - "the wire JSON is correct but tests reading it through the embedding struct see zero values"
---

# A Custom UnmarshalJSON Is Promoted Through Struct Embedding

## Symptom

The listener returned `{"event_id": …, "recipient": "ses_target"}` on the wire, but the test that
decoded the body into `struct { contracts.Envelope; Recipient string }` read `Recipient == ""`.
Same for `holder` on the publish response. Marshal was unaffected; only decode broke.

## Mechanism

Go promotes methods of an embedded struct to the outer struct. When `contracts.Envelope` gained a
generated `UnmarshalJSON` (to track which optional strings were present on the wire),
`json.Unmarshal` into any struct that **embeds** `Envelope` called the promoted method — which
knows nothing about the outer struct's fields and silently dropped them.

## Fix

- Response types compose `Envelope` as a named field or decode into a separate struct; tests never
  decode through an embedding struct.
- The generated method carries a doc comment naming the hazard.
- The presence-tracking `UnmarshalJSON` was later removed altogether: the only production decode of
  `Envelope` reads Go-produced envelopes off NATS, and client input enters through the listener's
  request types. Empty-string rejection now lives at the API boundary (`messageBody` validation
  with an `expected` field) and in a small `ValidateWire` over the raw ingress bytes, so no
  custom unmarshal is needed on the shared type.

## Lesson

Adding `UnmarshalJSON` to a widely embedded type changes decode behaviour for every embedder.
Before adding one, `grep` for the type name in struct literals and embedded fields; prefer
validating at the boundary that actually receives untrusted bytes.
