---
title: "Bun Fetch Mocking Patterns"
category: testing
tags:
  - bun
  - fetch
  - mocking
  - fire-and-forget
  - preconnect
date: 2026-04-04
status: active
module: daemon
related_issues:
  - "#199"
symptoms:
  - "Property 'preconnect' is missing in type"
  - "globalThis.fetch mock type error"
  - "How to test fire-and-forget fetch calls in Bun"
---

# Bun Fetch Mocking Patterns

## The `preconnect` Gotcha

In TypeScript 5.9+ with Bun types, `globalThis.fetch` includes a `preconnect` static
method. Assigning a plain async function to `globalThis.fetch` causes:

```
error TS2741: Property 'preconnect' is missing in type
  '(input: string | URL | Request, init?: RequestInit) => Promise<Response>'
  but required in type 'typeof fetch'.
```

### Fix: `Object.assign` with Original Properties

```typescript
const originalFetch = globalThis.fetch;

function mockFetch(interceptor: typeof fetch) {
  globalThis.fetch = Object.assign(interceptor, {
    preconnect: originalFetch.preconnect,
  });
}
```

Always restore in `afterEach`:

```typescript
afterEach(() => {
  globalThis.fetch = originalFetch;
});
```

## Testing Fire-and-Forget Fetch Calls

Fire-and-forget functions (like `subscribeWorkerToEnvoy`) use `.then().catch()` without
`await`. The promise resolves asynchronously after the caller returns.

### Pattern: Capture Array + `Bun.sleep`

```typescript
interface CapturedCall {
  url: string;
  body: unknown;
}

function mockFetchCapture(calls: CapturedCall[], statusCode = 200) {
  const mockFn = async (input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === "string"
      ? input
      : input instanceof URL ? input.href : input.url;
    if (url.includes("/v1/interests/subscribe")) {
      calls.push({ url, body: JSON.parse(init?.body as string) });
      return new Response("{}", { status: statusCode });
    }
    return originalFetch(input, init);
  };
  globalThis.fetch = Object.assign(mockFn, {
    preconnect: originalFetch.preconnect,
  });
}

it("calls Envoy subscribe after dispatch", async () => {
  const calls: CapturedCall[] = [];
  mockFetchCapture(calls);

  const response = await requestJson("/workers", { /* ... */ });
  expect(response.status).toBe(200);

  // Flush fire-and-forget microtasks
  await Bun.sleep(50);

  expect(calls).toHaveLength(1);
  expect(calls[0].body).toEqual({ session_id: "...", topics: ["..."] });
});
```

### Key Points

- **50ms sleep** is sufficient to flush promise chains from fire-and-forget calls.
- **Capture array** avoids global state — each test gets its own array.
- **Selective interception**: Only intercept URLs matching the target; pass everything else to `originalFetch`.
- **Test failure modes**: Use `statusCode = 500` for HTTP errors and `throw new Error()` for network errors. Assert the caller still returns 200.
