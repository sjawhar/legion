import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { getConnectionState, setConnectionState, useConnectionState } from "../api/live";
import { useEventStream } from "../api/sse";
import { countWholeCacheRefreshes, openStreamResponse } from "./sse-test-harness";

test("a stale reconnect timer never opens a second stream once online preempts it", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: string[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = String(input);
      streamCalls.push(url);
      if (streamCalls.length === 1) {
        // The first connection attempt fails outright (a dropped connection), which
        // schedules a reconnect on the normal 1s backoff timer.
        return new Response(null, { status: 503 });
      }
      // Every attempt after the first succeeds and stays open.
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;

    await waitFor(() => expect(streamCalls.length).toBe(1));

    // The network recovers before the 1s backoff elapses — this must connect
    // immediately instead of waiting out the pending timer.
    window.dispatchEvent(new Event("online"));
    await waitFor(() => expect(streamCalls.length).toBe(2));

    // Real-clock wait past the first attempt's backoff deadline (reconnectDelayMs(0)
    // = 1000ms): if the stale timer were not cleared when `online` preempted it, it
    // would fire here and open a second, duplicate stream. This genuinely needs to
    // wait against the platform clock — there is no synchronous signal for "a timer
    // did not fire".
    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 1_300);
    await settled;
    expect(streamCalls.length).toBe(2);
  } finally {
    unmount?.();
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("the watchdog reconnects a connection that opens and then goes silent without erroring", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: number[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (
      _input: RequestInfo | URL,
      init?: RequestInit
    ): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      // Every attempt "succeeds" (200 OK, onOpen fires) but never sends a byte — no
      // heartbeat, no event. There is no error to catch here; only the watchdog
      // (50ms, injected for this test in place of the real 45s) can detect it.
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    unmount = renderHook(() => useEventStream({ watchdogMs: 50 }), { wrapper: Wrapper }).unmount;

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(streamCalls.length).toBeGreaterThanOrEqual(2), { timeout: 2_000 });
  } finally {
    unmount?.();
    globalThis.fetch = originalFetch;
  }
});

test("the watchdog retries a request that never receives response headers", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: number[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wholeCache = countWholeCacheRefreshes(queryClient);

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (
      _input: RequestInfo | URL,
      init?: RequestInit
    ): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      if (streamCalls.length === 1) {
        return new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener("abort", () => {
            reject(new DOMException("Aborted", "AbortError"));
          });
        });
      }
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    unmount = renderHook(() => useEventStream({ watchdogMs: 50 }), { wrapper: Wrapper }).unmount;

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(streamCalls.length).toBe(2), { timeout: 2_000 });
    await waitFor(() => expect(wholeCache.count()).toBe(1));
  } finally {
    unmount?.();
    wholeCache.restore();
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("a 401 on the stream invalidates whoami and schedules no reconnect", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: number[] = [];
  const invalidated: unknown[][] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const originalInvalidate = queryClient.invalidateQueries.bind(queryClient);
  queryClient.invalidateQueries = (async (filters?: { queryKey?: readonly unknown[] }) => {
    if (filters?.queryKey !== undefined) {
      invalidated.push([...filters.queryKey]);
    }
    return originalInvalidate(filters);
  }) as typeof queryClient.invalidateQueries;

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (_input: RequestInfo | URL): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      // An expired/revoked session — an auth outcome, not a transport failure.
      return new Response(null, { status: 401 });
    }) as typeof fetch;

    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(invalidated).toContainEqual(["whoami"]));

    // No reconnect on an auth failure: waiting past the normal 1s backoff window
    // must not produce a second stream attempt — retrying can't succeed until the
    // user signs back in, so scheduling one would just spin forever.
    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 1_300);
    await settled;
    expect(streamCalls.length).toBe(1);
  } finally {
    unmount?.();
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("a non-auth 4xx on the stream marks the connection unavailable and schedules no reconnect", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: number[] = [];
  const invalidated: unknown[][] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const originalInvalidate = queryClient.invalidateQueries.bind(queryClient);
  queryClient.invalidateQueries = (async (filters?: { queryKey?: readonly unknown[] }) => {
    if (filters?.queryKey !== undefined) {
      invalidated.push([...filters.queryKey]);
    }
    return originalInvalidate(filters);
  }) as typeof queryClient.invalidateQueries;

  // Mirrors app.tsx's pill so the assertions below exercise what a reader
  // actually sees (text + a Reload control), not just the internal store.
  function ConnectionPillProbe(): ReactNode {
    const connection = useConnectionState();
    useEventStream();
    if (connection !== "unavailable") {
      return null;
    }
    return (
      <p>
        Live updates unavailable
        <button type="button">Reload</button>
      </p>
    );
  }

  setConnectionState("connected");

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (_input: RequestInfo | URL): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      // A 404: the request itself can never succeed by retrying it unchanged.
      return new Response(null, { status: 404 });
    }) as typeof fetch;

    unmount = render(
      <QueryClientProvider client={queryClient}>
        <ConnectionPillProbe />
      </QueryClientProvider>
    ).unmount;

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(getConnectionState()).toBe("unavailable"));
    expect(screen.getByText("Live updates unavailable")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();

    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 1_300);
    await settled;
    expect(streamCalls.length).toBe(1);
    expect(invalidated).not.toContainEqual(["whoami"]);

    // Terminal means terminal: a later online/visibilitychange must not reopen
    // the stream. The pill (and the state behind it) only clears on a real reload.
    window.dispatchEvent(new Event("online"));
    document.dispatchEvent(new Event("visibilitychange"));
    const { promise: settledAgain, resolve: settleAgain } = Promise.withResolvers<void>();
    setTimeout(settleAgain, 200);
    await settledAgain;
    expect(streamCalls.length).toBe(1);
    expect(getConnectionState()).toBe("unavailable");
    expect(screen.getByText("Live updates unavailable")).toBeTruthy();
  } finally {
    unmount?.();
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("a 429 on the stream keeps reconnecting instead of going terminal", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: number[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (
      _input: RequestInfo | URL,
      init?: RequestInit
    ): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      if (streamCalls.length === 1) {
        return new Response(null, { status: 429 });
      }
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(streamCalls.length).toBe(2), { timeout: 2_000 });
  } finally {
    unmount?.();
    globalThis.fetch = originalFetch;
  }
}, 10_000);
