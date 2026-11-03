---
title: "Validate once is a type whose only constructors validate, and the exported functions keep their messages"
category: envoy
tags:
  - validation
  - pmdoc
  - performance
  - error-messages
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2479"
---

# Validate once is a type whose only constructors validate, and the exported functions keep their messages

When several functions each validate the same immutable value and one caller runs them in a row:

- **Give the validated value its own type**, with an unexported field, and make the validating
  functions its only constructors. Give it methods holding the old bodies without the validation.
  Its doc comment states the holder's contract: nothing changes the value after it was validated.
- **Keep every exported function validating**, as the constructor followed by the method, so every
  caller outside the hot path refuses exactly as before.
- **Pass the caller's operation name into the constructor** when the old refusal names the
  function that refused (`"%s wants a document"`). Every wrapper then keeps its byte-exact message,
  and the hot path keeps the message of the first function it used to call.
- **Make the zero value return its exported counterpart's error for a nil input**, never a panic
  or an empty answer, and test it.
- **A function that also guards something else** (here the renderer's own refusals and its panic
  recovery) keeps that guard on both entries. Only the shared validation is skipped.

## Evidence

- `pmdoc/valid_document.go:14` `ValidDocument`, and `:22` `ValidateDocument(doc, operation)`, the
  former `wantDocument`. `ReadUnshared` (`pmdoc/read.go:31`) is the only other constructor.
- `pmdoc/token.go:52` `ValidDocument.TokenJSON` sends a zero value to `EncodeTokenJSON(nil, …)`.
  `pmdoc/valid_document_test.go` checks that each method on the zero value gives its exported
  counterpart's error.
- `pmdoc/render.go` `renderDocument(doc, spanCells, validate)` keeps `recoverPanic` and the
  `Render wants a doc` messages on both entries.
- `docs/read_cache.go:362` `foldLoaded` validates once. In the load-test profiles, `Validate` under
  `foldDocument` had 4 callers (1.74-2.09 s) at base and 1 (0.28-0.45 s) at head.
- `api/schema_repair_test.go` pins the exact refusal of 9 broken stored trees on every route, and
  the same file passed at base, at head and at main.
