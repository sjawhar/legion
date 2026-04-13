---
title: "Mock Envoy server pattern for daemon test isolation"
category: testing
tags:
  - test-isolation
  - mock-server
  - envoy
  - bun
  - fetch
  - fire-and-forget
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "#505"
symptoms:
  - "test sessions subscribing to live Envoy listener"
  - "405 delivery failures from dead test sessions"
  - "ses_test, ses_w1, ses_w2 appearing in live Envoy KV"
  - "test polluting production Envoy state"
---

# Mock Envoy Server Pattern for Daemon Test Isolation

## Problem

Daemon integration tests (`server.test.ts`, `index.test.ts`, `advance.test.ts`,
`session-id-contract.test.ts`, `integration.test.ts`) used test fixtures with session IDs
like `ses_test`, `ses_w1`. These subscribed to the **live** Envoy listener at
`localhost:9020` because `envoyUrl` defaulted to the production endpoint when not explicitly
set. This caused 405 delivery failures when Envoy tried to deliver to non-existent sessions.

Previous approach used `globalThis.fetch` interception (see `bun-fetch-mocking-patterns.md`)
which was fragile: required URL pattern matching, `preconnect` property preservation, and
separate mock helpers duplicated across test files.

## Solution: Per-Test Ephemeral Mock Server

### Production guard

Add early-return guards in all Envoy helper functions:

```typescript
function subscribeWorkerToEnvoy(envoyUrl: string, ...): void {
  if (!envoyUrl) return;  // tests pass "" or mock URL
  // ... actual network call
}
```

This is lighter than dependency injection for fire-and-forget calls — callers don't need to
change signatures.

### Mock server (`mock-envoy-server.ts`)

`createMockEnvoyServer()` spins up a real `Bun.serve()` on an ephemeral port (`port: 0`)
implementing the 4 Envoy endpoints (subscribe, unsubscribe, publish, roles/set). Each test
gets its own server instance — full isolation with real HTTP behavior.

```typescript
const mockEnvoy = await createMockEnvoyServer();

// Point tests at mock
await startTestServer({ envoyUrl: mockEnvoy.url });

// Assert on structured calls (not parsed fetch bodies)
expect(mockEnvoy.subscribeCalls[0].session_id).toBe("ses_w1");

// Simulate errors
mockEnvoy.subscribeStatus = 500;  // HTTP error (server still running)
mockEnvoy.stop();                  // Network error (connection refused)

// Cleanup
afterEach(() => mockEnvoy.stop());
```

### Key advantages over fetch interception

| Aspect | Fetch interception | Mock server |
|--------|--------------------|-------------|
| Fidelity | Synthetic responses | Real HTTP round trips |
| Type safety | Manual URL parsing | Structured typed call records |
| Isolation | Global `fetch` override | Per-test server instance |
| Error sim | `throw new Error()` | `stop()` for real connection refused |
| Boilerplate | `Object.assign(mockFn, { preconnect })` | None |

### When fetch interception is still needed

The mock server can't simulate every failure mode. Fetch interception remains appropriate for:
- **Fire-and-forget network errors**: When the function being tested doesn't `await` the
  fetch and you need to verify errors are swallowed (e.g., `publishStateDelta` network
  failure test — you can't simulate a rejected `fetch()` with a running server)

## Gotcha: Fire-and-forget flush timing

Many Envoy calls are fire-and-forget (no `await`). Tests need `await Bun.sleep(50)` to let
promises flush before asserting on recorded calls. This is inherently fragile — consider
polling `mockEnvoy.subscribeCalls.length` until stable, with a timeout, for flake resistance.

## Related

- `bun-fetch-mocking-patterns.md` — the older fetch interception approach (still valid for
  non-Envoy fetch mocks, and for the specific network error cases above)
- `sse-mock-initialization-order.md` — mock HTTP/SSE server patterns for Envoy MCP bridge
- `nats-kv-testing-patterns.md` — real container patterns for NATS KV testing
