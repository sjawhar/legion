---
title: "A read helper that swallows a failed status makes an unchanged assertion vacuous"
category: testing
tags:
  - go
  - httptest
  - negative-control
  - test-helpers
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2546"
---

# A read helper that swallows a failed status makes an unchanged assertion vacuous

Extends docs/solutions/best-practices/a-negative-control-that-cites-an-existing-test-is-read-before-it-is-cited-INFRA-188.md.

That document says an assertion about absence can pass for nothing. This is the helper that makes
it happen, and it looks like ordinary test plumbing.

- A helper a test uses to read a server surface asserts the status before it returns a body. One
  that decodes whatever came back returns the zero value on a failed read, and every
  `after != before` comparison then holds `"" == ""`.
- An assertion that a write changed nothing is the one that this silences, because both of its
  reads fail together. The assertion that a write did something still fails, so the test looks
  partly alive.
- Never keep two read helpers for one surface where one checks the status and one does not. Delete
  the blind one in the same change; a copy left in the package is picked up by the next test.
- Prove the fix by pointing the first read at an id the server does not hold: the test must fail
  there, naming the status. Record that output, because it is the only evidence that the
  comparison is now reachable.

## Evidence

`TestSetBlockAttributesRefusesABlockPointerInTheLiveRoom`
(`packages/envoy/internal/dispatch/api/block_pointers_test.go`) was written with a local
`artifactMarkdown` that called `decodeBody` on the response without reading its code. Each of its
four refusal rows ends in `if after := documentMarkdown(t, handler, saved.ID); after != before`,
the claim that the live room keeps nothing of a refused write. With the blind helper, two failed
reads satisfied that claim.

The package already had `documentMarkdown`
(`packages/envoy/internal/dispatch/api/ask_block_edit_settlement_test.go:94`), which fails on any
status but 200. All three reads now use it and the copy is deleted.

The control, run in the issue's pod: with the first read pointed at
`11111111-1111-4111-8111-111111111111`, the test fails at `block_pointers_test.go:221` with
`read document text: status=404 body={"code":"ARTIFACT_NOT_FOUND","error":"artifact not found"}`.
Under the deleted helper that read returned the empty string and the row passed.

A reviewer found this, not a test run: no suite can report an assertion that holds for the wrong
reason.
