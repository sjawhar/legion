---
title: "Audit all call sites when fixing test isolation, not just helpers"
category: testing
tags:
  - test-isolation
  - audit-pattern
  - code-review
  - testing
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "#505"
symptoms:
  - "test isolation fix missed some call sites"
  - "some tests still hitting live endpoint after fix"
  - "helper was fixed but direct calls were not"
---

# Audit All Call Sites When Fixing Test Isolation

## Problem

When fixing test isolation for Envoy in `server.test.ts`, the initial audit focused on
`startTestServer()` — the centralized test helper. But the file had 7 `startServer()` calls
total: 1 in the helper and 6 direct calls in the "dead worker cleanup" and "shutdown" test
sections. The 6 direct calls were missed in earlier review rounds because the audit scope
was "the test helper", not "every call to the function that creates the system under test."

## Rule: The audit unit is "every call to the function being isolated"

When fixing a test isolation issue, the very first step should be:

```bash
grep -n 'startServer(' packages/daemon/src/daemon/__tests__/server.test.ts
```

Build the audit table from that grep output, not from reading the test helpers:

| File | `startServer()` calls | Envoy isolation |
|------|----------------------|-----------------| 
| `server.test.ts` | 7 (1 helper + 6 direct) | All use `mockEnvoy.url` |
| `advance.test.ts` | 2 | Both use `mockEnvoy.url` |
| `integration.test.ts` | 1 | Uses `mockEnvoy.url` |
| `session-id-contract.test.ts` | 3 | All use `mockEnvoy.url` |
| `index.test.ts` | via `startDaemonForTest` | Uses `currentMockEnvoy.url` |

This table should be the **starting point** of implementation, not a post-hoc verification.

## Pattern: Systematic call-site audit

1. **Identify the isolation boundary**: What function/constructor creates the thing that
   connects to the external service? (e.g., `startServer()`, not `startTestServer()`)
2. **Grep for ALL call sites** across all test files, not just the helper
3. **Build the audit table** before writing any code
4. **Verify every row** in the table is addressed in the PR
5. **Include the table** in the PR description for reviewers

## Why helpers mislead

Test helpers (`startTestServer`, `startDaemonForTest`) are the natural audit target because
they're the "right way" to do things. But tests that predate the helper, or tests in unusual
sections (shutdown, error recovery, cleanup) often call the underlying function directly. The
helper-centric audit creates a blind spot for these direct calls.

## Defensive measure: Required parameters

The root design issue is that `envoyUrl` defaults to the production endpoint:

```typescript
// Dangerous — forgetting to pass envoyUrl silently does the wrong thing
function startServer(opts: { envoyUrl?: string }) { ... }
```

Making the parameter required forces every caller to be explicit:

```typescript
// Safe — every caller must choose explicitly
function startServer(opts: { envoyUrl: string }) { ... }
```

When this isn't practical (breaking change across many callers), the early-return guard
(`if (!envoyUrl) return`) combined with the call-site audit is the next best option.
