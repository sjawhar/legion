---
title: "A red-before on a twice-validated path removes the arm that writes the message"
category: testing
tags:
  - go
  - negative-control
  - validation
  - pmdoc
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2546"
---

# A red-before on a twice-validated path removes the arm that writes the message

Extends docs/solutions/envoy/a-fan-out-copy-defers-to-a-direct-send-only-once-that-send-was-recorded-delivered-and-the-wait-is-bounded-by-the-senders-own-lease-pr2225.md.

That document says to aim a negative control at the code that decides the behaviour. Where one
write validates the same tree more than once, no single validator is that code, and a control that
removes one of them stays green while proving nothing.

- List every guard the value passes on the path before you write the control. A package that
  re-validates the whole document after each mutation has one guard per mutation helper and
  another in the write itself.
- Remove the **arm that produces the refusal's sentence**, not a validator, when the sentence is
  what the test pins. The value is then refused generically, and the test fails on its own
  wording.
- A removal that leaves the test green is a reading, not a failure of the test: record which
  single removals stayed green and why, so the next reader does not repeat them.
- Make the test say which layer refused. Refused-as-a-value and refused-as-a-broken-document are
  two behaviours, and a test that accepts either cannot tell a working guard from a write the room
  recorded and complained about afterwards.

## Evidence

`TestSetBlockAttributesRefusesABlockPointerInTheLiveRoom`
(`packages/envoy/internal/dispatch/api/block_pointers_test.go`) pins the sentence
`is a block of an ask: a block[] attribute points only at a block of a document` on the live
room's own write.

Removing `pmdoc.SetBlockAttributes`' `out.Validate()`
(`packages/envoy/internal/dispatch/pmdoc/retype.go:127`) leaves the test green: `pmdoc.Update`
validates the same tree again (`pmdoc/update.go:22`). Removing both makes the room record the
value and refuse its own document on the next read, which is a different behaviour and the one the
test's own assertion rejects.

The control that works removes the ask arm of `validateBlockPointerValues`
(`packages/envoy/internal/dispatch/pmdoc/schema.go:372`) with everything else intact. Both ask rows
then fail, reporting the generic
`is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, …` in place of
the sentence.

The test separates the layers with
`if !errors.Is(err, pmdoc.ErrSchema) || errors.Is(err, docs.ErrDocSchema)`, so "the value was
refused" is never satisfied by "the document is outside the schema".
