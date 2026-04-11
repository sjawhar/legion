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
  - "fetchGitHubProjectItems tests broken by unrelated test file"
  - "mock.module replaces module globally across test files"
  - "Bun test isolation failure with module mocks"
---

# Bun mock.module() Leaks Globally — Use Dependency Injection Instead

## Problem

`mock.module()` in Bun replaces the module registry **globally and permanently** within a
test run. Unlike `globalThis.fetch` mocks (which can be restored in `afterEach`), module
mocks cannot be undone — they persist across all test files in the same process.

This means if `server.test.ts` calls `mock.module("../../state/github-fetch", ...)`, then
`github-fetch.test.ts` (which tests the **real** `fetchGitHubProjectItems`) will get the
mock instead of the real implementation. Tests pass in isolation but fail in CI where all
test files run in one process.

## Symptoms

- Tests pass when running a single file: `bun test server.test.ts` ✅
- Tests fail when running the full suite: `bun test packages/daemon/` ❌
- The failing tests are in a **different file** from where `mock.module()` was called
- Error messages suggest the function returns unexpected values (from the mock, not the real implementation)

## When This Happens

Any function that uses `Bun.spawn` internally (like `fetchGitHubProjectItems` which shells
out to `gh`) cannot be mocked via `globalThis.fetch`. The natural instinct is to reach for
`mock.module()` — but that creates the global leak.

## Solution: Dependency Injection via ServerOptions

Instead of module-level mocking, inject the function via the options interface:

### 1. Add optional function to ServerOptions

```typescript
export interface ServerOptions {
  // ... existing fields ...
  /** Injectable fetcher for testing — defaults to fetchGitHubProjectItems */
  fetchProjectItems?: (owner: string, projectNumber: number) => Promise<unknown>;
}
```

### 2. Use injected function with fallback in handler

```typescript
const fetchFn = opts.fetchProjectItems ?? fetchGitHubProjectItems;
const rawIssues = await fetchFn(boardParts[0], projectNumber);
```

### 3. Pass mock via test helper

```typescript
function makeFetchProjectItems(boardMocks: Map<string, unknown>) {
  return async (owner: string, projectNumber: number) => {
    const key = `${owner}/${projectNumber}`;
    const result = boardMocks.get(key);
    if (result instanceof Error) throw result;    // simulate board failure
    if (result === undefined) throw new Error(`No mock for board ${key}`);
    return result;
  };
}

// In test:
const boardMocks = new Map([
  ["acme/123", [makeGitHubProjectItem("acme/widgets", 10, "Todo")]],
]);
await startTestServer({
  legionId: "acme/123",
  fetchProjectItems: makeFetchProjectItems(boardMocks),
});
```

## Rule

**Never use `mock.module()` in this codebase.** Always prefer dependency injection via
`ServerOptions` (or equivalent options interface) for functions that use `Bun.spawn` or
other non-interceptable internals.

For `globalThis.fetch` mocking, continue using the `Object.assign` pattern documented in
`testing/bun-fetch-mocking-patterns.md` — those mocks CAN be restored in `afterEach`.

## Related

- `testing/bun-fetch-mocking-patterns.md` — safe patterns for `globalThis.fetch` mocking
- `testing/github-backend-collect-test-payloads.md` — exact payload shapes for `/state/collect`
