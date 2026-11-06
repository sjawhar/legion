---
title: "An optional wire field is pinned by presence, in a lane that always runs"
category: testing
tags:
  - wire-contracts
  - go
  - typescript
  - ci-lanes
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2508"
---

# An optional wire field is pinned by presence, in a lane that always runs

A field a producer adds and a consumer reads as optional has three states, and two of them are the
regression: absent, and present with the wrong value. A test that decodes it into a zero-valued
`bool` or reads it off a hand-built fixture can tell neither.

- Assert **presence** before value on the producer's own output: decode into
  `map[string]json.RawMessage`, a `*bool`, or whatever your language's "was it there" is, and fail
  when the key is missing. A plain `bool` reads a missing field as `false`, which is the shape a
  dropped field has.
- Build the frame the assertion reads from the **producer**, never in the test: a consumer-side row
  that constructs its own frame pins the consumer's reading of a contract the producer may have
  stopped honouring.
- Put at least one producer-side presence assertion in a lane **that always runs**. A check that
  lives only in a lane needing a container runtime, a cluster, or a path filter the change does not
  trip is no guard: the contract then rests on a test nobody runs on the pull request.
- Declare the transport's extra field as its own type rather than widening the shared one, when a
  fixture or a key list pins that shared shape. The derived type costs a line; widening forces an
  exception into the pin that protects every other copy of the value.
- Read the optional field as three states in the consumer, and give the off-nominal shapes their
  own rows (absent, `null`, a non-boolean), because the fallback branch is what carries the old
  behaviour.

## Evidence

sjawhar/legion#2508 put `quiet` on every event Dispatch's outbox publishes and had the TypeScript renderer
judge it, falling back to `notify` when it is absent so the 72 hours the stream retains stay as
they were (`packages/envoy-client/src/delivery.ts`). The fallback is what makes absence
indistinguishable from `false` at the consumer, and so what makes a producer-side presence
assertion the only guard.

`requireCommentQuietPublished` (`packages/envoy/infra/core/dispatch_permissions_live_test.go`)
decodes the frame into `map[string]json.RawMessage` and asserts the key is there before its value,
with the reason in its own comment. The Go unit test beside it decodes into a struct with a plain
`bool`, and the TypeScript rows build their own frames — so adding `,omitempty` to the producer's
field would leave every test that runs on a pull request green while restoring the original defect
in full. That presence assertion lives in the `envoy-infra-go` lane, which needs a container
runtime and which `main`'s ruleset does not require.

For the typed side: `PublishedDispatchEvent = DispatchEvent & { readonly quiet?: boolean }` in
`packages/contracts/src/dispatch-api.ts`. Declaring `quiet` on `DispatchEventBase` instead would
have forced an `Exclude<>` exception into `src/live.test.ts`, which pins that interface key-for-key
against `fixtures/live/event.json` — a field the API never sends corroding the test that protects
the API's own shape.
