---
title: "A test hook the server links imports no test package, and shares crafted bytes through a leaf package"
category: testing
tags:
  - go
  - test-helpers
  - imports
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2448"
---

# A test hook the server links imports no test package, and shares crafted bytes through a leaf package

- A fixture that tests in more than one package need, and that must reach a package's unexported
  code, goes in a non-test file such as `docs/testhooks.go`, with a `ForTest` name. The server
  binary links that file, so it imports neither `testing` nor a helper package that imports it
  (`docs/docstest`, `store/storetest`).
- Code both the hook and the test helpers need, such as building a crafted ygo update, goes in a
  small leaf package that imports neither `testing` nor any Dispatch package (`docs/crafted`). The
  hook calls it. The test helper (`docstest.NestedLinkUpdate`) wraps it and fails the test on its
  error. Do not copy the code into the hook: you cannot import across the boundary, but a third
  package removes the copy.
- Move every caller of a moved helper to the leaf package. Do not leave aliases behind.
- Check the boundary with `go list -deps ./cmd/dispatch`: `docstest` and `storetest` must not
  appear. `testing` itself already appears, through a third-party ygo package, so do not grep for
  it as the check.

## Evidence

- sjawhar/legion#2448: 9bac6d86 moved the outside-schema corruption builders into
  `docs/testhooks.go` (`OutsideSchemaCorruptionsForTest`, used by `api/schema_repair_test.go` and
  the docs equivalence test). It copied about 60 lines of `docstest.NestedLinkUpdate`'s byte
  surgery into that server-linked file. Review thread PRRT_kwDORFy7ds6xNCXs flagged the copy, and
  f5035f9e moved `NestedLinkUpdate`, `FirstText` and `NestedValue` into `docs/crafted`. It also
  moved their callers in `docs/history_fold_test.go`, `docs/settle_depth_test.go` and
  `pmdoc/nesting_test.go` there.
