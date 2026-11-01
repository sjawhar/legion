---
title: "Bun mock.module() Leaks Globally — Use Dependency Injection Instead"
category: testing
tags:
  - bun
  - mocking
  - mock-module
  - dependency-injection
  - test-isolation
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "#92"
symptoms:
  - "Tests in other files fail after mock.module() in one test file"
  - "An external collector test is affected by an unrelated test file"
  - "mock.module replaces module globally across test files"
  - "Bun test isolation failure with module mocks"
---

# Bun mock.module() Leaks Globally — Use Dependency Injection Instead

## Problem

`mock.module()` in Bun replaces the module registry **globally and permanently** within a
test run. Unlike `globalThis.fetch` mocks (which can be restored in `afterEach`), module
mocks cannot be undone — they persist across all test files in the same process.

This means a test that calls `mock.module()` for a shared dependency can change what a later test
imports. Tests pass in isolation but fail when the full suite loads both files into one process.

## Symptoms

- Tests pass when running a single file: `bun test server.test.ts` ✅
- Tests fail when running the full suite: `bun test packages/daemon/` ❌
- The failing tests are in a **different file** from where `mock.module()` was called
- Error messages suggest the function returns unexpected values (from the mock, not the real implementation)

## When This Happens

Any function that shells out or reaches a non-interceptable dependency cannot be safely replaced
with `globalThis.fetch`. Replacing its module registry leaks that replacement to other tests.

## Solution: dependency injection

Make the external operation an explicit dependency of the subsystem that consumes it. For example,
`RunResyncDeps` receives both the Dispatch client and the CI-status reader, so a test provides only
the controlled response it needs while the resync reducer and persistence path remain real.

Keep the dependency at the boundary that owns the side effect, not in a module-level test mock.
The caller's test helper can provide a narrow function or client double; other test files then keep
their own real imports and cannot observe the replacement.

## Rule

**Never use `mock.module()` in this codebase.** Always prefer dependency injection via
`ServerOptions` (or equivalent options interface) for functions that use `Bun.spawn` or
other non-interceptable internals.

For `globalThis.fetch` mocking, continue using the `Object.assign` pattern documented in
`testing/bun-fetch-mocking-patterns.md` — those mocks CAN be restored in `afterEach`.

## Related

- `testing/bun-fetch-mocking-patterns.md` — safe patterns for `globalThis.fetch` mocking
- `testing/github-backend-collect-test-payloads.md` — exact payload shapes for `/state/collect`
