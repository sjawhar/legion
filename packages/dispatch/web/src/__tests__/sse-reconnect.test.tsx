import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, QueryObserver, useQuery } from "@tanstack/react-query";
import { render, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { getConnectionState, setConnectionState, useConnectionState } from "../api/live";
import { useEventStream, WHOLE_CACHE_REFRESH_MS } from "../api/sse";

function openStreamResponse(signal: AbortSignal | null | undefined, ...frames: string[]): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const encoder = new TextEncoder();
      for (const frame of frames) {
        controller.enqueue(encoder.encode(frame));
      }
      // Never close on its own — this simulates a live, connected stream that stays
      // open until the client aborts it.
      signal?.addEventListener("abort", () => {
        controller.error(new DOMException("Aborted", "AbortError"));
      });
    },
  });
  return new Response(body, { status: 200 });
}

function issueUpdatedFrame(id: number, title = `title-${id}`): string {
  const event = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-23T00:00:00Z",
    id,
    issue_key: "CORE-1",
    notify: true,
    payload: { key: "CORE-1", title },
    project: "CORE",
    seq: id,
    type: "issue.updated",
  };
  return `id: ${id}\nevent: issue.updated\ndata: ${JSON.stringify(event)}\n\n`;
}

function emittingStream(onEmit: (emit: (frame: string) => void) => void): typeof fetch {
  return (async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        onEmit((frame) => controller.enqueue(new TextEncoder().encode(frame)));
        init?.signal?.addEventListener("abort", () => {
          controller.error(new DOMException("Aborted", "AbortError"));
        });
      },
    });
    return new Response(body, { status: 200 });
  }) as typeof fetch;
}

test("a stale reconnect timer never opens a second stream once online preempts it", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: string[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

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

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });

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

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("the no-chunk watchdog reconnects a connection that goes silent without erroring", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: number[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

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

    const { unmount } = renderHook(() => useEventStream(50), { wrapper: Wrapper });

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(streamCalls.length).toBeGreaterThanOrEqual(2), { timeout: 2_000 });

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
});

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

  try {
    globalThis.fetch = (async (_input: RequestInfo | URL): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      // An expired/revoked session — an auth outcome, not a transport failure.
      return new Response(null, { status: 401 });
    }) as typeof fetch;

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(invalidated).toContainEqual(["whoami"]));

    // No reconnect on an auth failure: waiting past the normal 1s backoff window
    // must not produce a second stream attempt — retrying can't succeed until the
    // user signs back in, so scheduling one would just spin forever.
    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 1_300);
    await settled;
    expect(streamCalls.length).toBe(1);

    unmount();
  } finally {
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

  try {
    globalThis.fetch = (async (_input: RequestInfo | URL): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      // A 404: the request itself can never succeed by retrying it unchanged.
      return new Response(null, { status: 404 });
    }) as typeof fetch;

    const { unmount } = render(
      <QueryClientProvider client={queryClient}>
        <ConnectionPillProbe />
      </QueryClientProvider>
    );

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

    unmount();
  } finally {
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

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(streamCalls.length).toBe(2), { timeout: 2_000 });

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("an out-of-order lower-id live event is applied and never regresses the reconnect cursor", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: string[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  const frame = (id: number) =>
    `id: ${id}\nevent: issue.updated\ndata: {"id":${id},"issue_key":"CORE-1","seq":${id},"type":"issue.updated","actor":{"kind":"session","id":"s"},"notify":false,"created_at":"2026-01-01T00:00:00Z","payload":{}}\n\n`;

  try {
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = String(input);
      streamCalls.push(url);
      if (streamCalls.length === 1) {
        // A higher id arrives first, then a lower one out of order — the same
        // stalled-transaction race TestSSELiveEventBelowSinceIsNotDropped proves
        // the server now forwards (Go) rather than filters by id.
        return openStreamResponse(init?.signal, frame(101), frame(100));
      }
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });

    await waitFor(() => expect(streamCalls.length).toBe(1));
    // The very first connection ever omits since entirely — the server
    // subscribes before resolving its own current head, so there is no
    // separate request left to race.
    expect(new URL(streamCalls[0], "http://localhost").searchParams.has("since")).toBe(false);
    // Let both already-enqueued frames drain through the reader loop — there is no
    // artificial delay between them, so this settles almost immediately.
    const { promise: drained, resolve: drain } = Promise.withResolvers<void>();
    setTimeout(drain, 150);
    await drained;

    // Force a reconnect: it must resume from 101 (the max id seen), not 100 (the
    // id most recently received, which arrived out of order and is lower).
    window.dispatchEvent(new Event("online"));
    await waitFor(() => expect(streamCalls.length).toBe(2));
    expect(new URL(streamCalls[1], "http://localhost").searchParams.get("since")).toBe("101");

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("visibilitychange while a stream is open replaces it once, resuming from the cursor", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: string[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  const frame = (id: number) =>
    `id: ${id}\nevent: issue.updated\ndata: {"id":${id},"issue_key":"CORE-1","seq":${id},"type":"issue.updated","actor":{"kind":"session","id":"s"},"notify":false,"created_at":"2026-01-01T00:00:00Z","payload":{}}\n\n`;

  try {
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = String(input);
      streamCalls.push(url);
      if (streamCalls.length === 1) {
        // One event, then the stream stays open until the client aborts it.
        return openStreamResponse(init?.signal, frame(7));
      }
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });

    await waitFor(() => expect(streamCalls.length).toBe(1));
    // Let the enqueued frame drain through the reader loop before forcing a reconnect.
    const { promise: drained, resolve: drain } = Promise.withResolvers<void>();
    setTimeout(drain, 150);
    await drained;

    // The tab comes back to the foreground (happy-dom reports `visible`): the open
    // stream is torn down and replaced immediately, resuming from the last id seen.
    document.dispatchEvent(new Event("visibilitychange"));
    await waitFor(() => expect(streamCalls.length).toBe(2));
    expect(new URL(streamCalls[1], "http://localhost").searchParams.get("since")).toBe("7");

    // One replacement, not two: the abort's own settle must not schedule a
    // second reopen on top of the forced one.
    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 200);
    await settled;
    expect(streamCalls.length).toBe(2);

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("a reconnect refreshes every rendered query, including ones no key list named", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: number[] = [];
  const fetches = { checks: 0, issues: 0 };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  // An infinite `staleTime` keeps TanStack's own refetch-on-reconnect out of the result: these
  // queries are never stale by time, so any refetch below is the stream's doing. The checks key
  // is one of the eight registered keys the hand-maintained reconnect list had drifted past - a
  // pull request's check rows on an open issue.
  function Probe(): ReactNode {
    useEventStream();
    useQuery({
      queryFn: async () => ++fetches.checks,
      queryKey: ["github-link-checks", "https://github.com/sjawhar/legion/pull/1", "c0ffee"],
      staleTime: Number.POSITIVE_INFINITY,
    });
    useQuery({
      queryFn: async () => ++fetches.issues,
      queryKey: ["issues", "project", "CORE"],
      staleTime: Number.POSITIVE_INFINITY,
    });
    return null;
  }

  try {
    globalThis.fetch = (async (
      _input: RequestInfo | URL,
      init?: RequestInit
    ): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    const { unmount } = render(
      <QueryClientProvider client={queryClient}>
        <Probe />
      </QueryClientProvider>
    );

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(fetches).toEqual({ checks: 1, issues: 1 }));
    // The very first open has nothing stale to refresh.
    const { promise: drained, resolve: drain } = Promise.withResolvers<void>();
    setTimeout(drain, 150);
    await drained;
    expect(fetches).toEqual({ checks: 1, issues: 1 });

    // A reconnect may have missed events the stream never saw (and the server's `since` replay
    // is not exhaustive), so every query the app holds refreshes - not only the ones some list
    // remembered to name.
    window.dispatchEvent(new Event("online"));
    await waitFor(() => expect(streamCalls.length).toBe(2));
    await waitFor(() => expect(fetches).toEqual({ checks: 2, issues: 2 }));

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("an event during a list's first load beats the response that predates it", async () => {
  const originalFetch = globalThis.fetch;
  // The production client's staleTime: a body that lands marked fresh stays on screen for half
  // a minute, which is what makes losing this race a stale screen rather than a blink.
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const titles = ["OLDTITLE", "NEWTITLE"];
  const { promise: held, resolve: release } = Promise.withResolvers<void>();
  let listCalls = 0;
  let emit: ((frame: string) => void) | undefined;

  function Probe(): ReactNode {
    useEventStream();
    const { data } = useQuery({
      queryFn: async () => {
        const attempt = listCalls++;
        if (attempt === 0) {
          await held;
        }
        return titles[attempt] ?? titles[titles.length - 1];
      },
      queryKey: ["issues", "project", "CORE"],
    });
    return <p>{data ?? "loading"}</p>;
  }

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });

    const { unmount } = render(
      <QueryClientProvider client={queryClient}>
        <Probe />
      </QueryClientProvider>
    );

    // The list's first request is in flight and its response is held open.
    await waitFor(() => expect(listCalls).toBe(1));
    await waitFor(() => expect(emit).toBeDefined());

    // The issue is renamed while that request is still out.
    emit?.(issueUpdatedFrame(1, "NEWTITLE"));

    // The invalidation issues a request of its own instead of waiting on the one already out.
    await waitFor(() => expect(listCalls).toBe(2));

    // Releasing the pre-event body must not put it back on screen, nor mark it fresh.
    release();
    await waitFor(() => expect(screen.getByText("NEWTITLE")).toBeTruthy());
    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 200);
    await settled;
    expect(screen.getByText("NEWTITLE")).toBeTruthy();

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("an unknown live event conservatively refreshes everything", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of [["issues"], ["inbox"], ["user-state"], ["user-agent-state"]]) {
    queryClient.setQueryData(key, { loaded: true });
  }

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  try {
    globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> =>
      openStreamResponse(init?.signal, "id: 1\nevent: future.event\ndata: {}\n\n")) as typeof fetch;

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });
    const isStale = (key: readonly unknown[]) =>
      queryClient.getQueryCache().find({ queryKey: key, exact: true })?.isStale();

    await waitFor(() => expect(isStale(["issues"])).toBe(true));
    expect(isStale(["inbox"])).toBe(true);
    expect(isStale(["user-state"])).toBe(true);
    // A key no hand-written list ever named refreshes too.
    expect(isStale(["user-agent-state"])).toBe(true);

    unmount();
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("a queued prefix key replaces the narrower key instead of refetching it twice", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // A `child.status` event queues both `["issue"]` and `["issue", "CORE-1"]`. React Query
  // matches by prefix and `invalidateQueries` defaults to `cancelRefetch: true`, so sending
  // both cancels this query's in-flight refetch and starts a second request for it.
  let issueFetches = 0;
  const observer = new QueryObserver(queryClient, {
    queryKey: ["issue", "CORE-1"],
    queryFn: async () => {
      issueFetches += 1;
      return { key: "CORE-1" };
    },
  });
  const unsubscribe = observer.subscribe(() => {});

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  const childStatus =
    'id: 1\nevent: child.status\ndata: {"id":1,"issue_key":"CORE-1","project":"CORE","seq":4,' +
    '"type":"child.status","actor":{"kind":"session","id":"s"},"notify":false,' +
    '"created_at":"2026-01-01T00:00:00Z","payload":{"child_key":"CORE-2"}}\n\n';

  try {
    await waitFor(() => expect(issueFetches).toBe(1));

    globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> =>
      openStreamResponse(init?.signal, childStatus)) as typeof fetch;

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });

    await waitFor(() => expect(issueFetches).toBe(2));
    // Without the prefix drop the cancelled refetch starts again and the count reaches 3.
    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 400);
    await settled;
    expect(issueFetches).toBe(2);

    unmount();
  } finally {
    unsubscribe();
    globalThis.fetch = originalFetch;
  }
}, 10_000);

/**
 * A list query whose every load is released by the test, so a scenario can say exactly which
 * load is in flight when an event arrives. `starts` counts loads; `release(n)` answers load n.
 */
/** The list surface both deferred-load scenarios render: the stream plus one list query. */
function ListProbe({ list }: { list: DeferredListQuery }): ReactNode {
  useEventStream();
  const { data } = useQuery({ queryFn: list.queryFn, queryKey: list.key });
  return <p>{data ?? "loading"}</p>;
}

interface DeferredListQuery {
  key: readonly unknown[];
  queryFn: () => Promise<string>;
  release: (load: number) => void;
  releaseAll: () => void;
  starts: () => number;
}

function deferredListQuery(): DeferredListQuery {
  const pending = new Map<number, (body: string) => void>();
  let starts = 0;
  return {
    key: ["issues", "project", "CORE"],
    queryFn: () => {
      const load = ++starts;
      return new Promise<string>((resolve) => pending.set(load, resolve));
    },
    release: (load: number) => pending.get(load)?.(`list-${load}`),
    // No load may outlive its test: a promise still pending at teardown keeps the runner's
    // event loop holding a query function from a torn-down render.
    releaseAll: () => {
      for (const [load, resolve] of pending) {
        resolve(`list-${load}`);
      }
      pending.clear();
    },
    starts: () => starts,
  };
}

test("a steady event trickle never starves a slow first load", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const list = deferredListQuery();
  let emit: ((frame: string) => void) | undefined;

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    const { unmount } = render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );
    await waitFor(() => expect(list.starts()).toBe(1));
    await waitFor(() => expect(emit).toBeDefined());

    // The one cancel this query is allowed: load 1 is reverted and load 2 takes its place.
    emit?.(issueUpdatedFrame(1));
    await waitFor(() => expect(list.starts()).toBe(2));

    // Every later event arrives while load 2 is still in flight. Cancelling on each of them is
    // what starved the reader; they register a settle refresh instead, and load 2 survives.
    for (let index = 2; index <= 5; index += 1) {
      emit?.(issueUpdatedFrame(index));
      await new Promise((resolve) => setTimeout(resolve, 150));
    }
    expect(list.starts()).toBe(2);

    // (a) The starvation fix: the held load lands and the reader leaves the skeleton.
    list.release(2);
    await waitFor(() => expect(screen.getByText("list-2")).toBeTruthy());

    // (b) Load 2's body predates the last event, so its settling owes a refresh: exactly one
    // more load starts, and its body is what the reader ends on.
    await waitFor(() => expect(list.starts()).toBe(3));
    list.release(3);
    await waitFor(() => expect(screen.getByText("list-3")).toBeTruthy());
    await new Promise((resolve) => setTimeout(resolve, 400));
    expect(list.starts()).toBe(3);

    unmount();
  } finally {
    list.releaseAll();
    globalThis.fetch = originalFetch;
  }
}, 20_000);

test("unknown frames refresh at most once per window, leading and trailing", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // A whole-cache refresh is the only invalidation that names no key; counting those counts
  // exactly the unknown-frame refreshes.
  const invalidate = queryClient.invalidateQueries.bind(queryClient);
  let refreshes = 0;
  queryClient.invalidateQueries = ((filters?: { queryKey?: readonly unknown[] }) => {
    if (filters?.queryKey === undefined) {
      refreshes += 1;
    }
    return invalidate(filters);
  }) as typeof queryClient.invalidateQueries;
  let emit: ((frame: string) => void) | undefined;

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });
    await waitFor(() => expect(emit).toBeDefined());

    // Fifty frames of a type this build cannot parse, inside one window. Unthrottled this is
    // fifty whole-cache refreshes, each of which reaches the GitHub proxy queries.
    for (let index = 1; index <= 50; index += 1) {
      emit?.(`id: ${index}\nevent: future.event\ndata: {}\n\n`);
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    await waitFor(() => expect(refreshes).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(refreshes).toBe(1);

    // One trailing refresh carries everything the burst asked for.
    await waitFor(() => expect(refreshes).toBe(2), { timeout: 8_000 });
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(refreshes).toBe(2);

    // And a frame after the window refreshes again: a second change of that same new type is
    // one this tab would otherwise never learn.
    await new Promise((resolve) => setTimeout(resolve, WHOLE_CACHE_REFRESH_MS));
    emit?.("id: 99\nevent: future.event\ndata: {}\n\n");
    await waitFor(() => expect(refreshes).toBe(3));

    unmount();
  } finally {
    queryClient.invalidateQueries = invalidate;
    globalThis.fetch = originalFetch;
  }
}, 40_000);

test("a query removed and recreated between flushes still has its first load cancelled", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const list = deferredListQuery();
  let emit: ((frame: string) => void) | undefined;
  const renderProbe = () =>
    render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    const signedIn = renderProbe();
    await waitFor(() => expect(list.starts()).toBe(1));
    await waitFor(() => expect(emit).toBeDefined());

    // Load 1 is cancelled, so this query's hash is recorded as already cancelled.
    emit?.(issueUpdatedFrame(1));
    await waitFor(() => expect(list.starts()).toBe(2));

    // Sign out and straight back in, with no flush in between: the recreated query is a fresh
    // first load, and a stale hash would have it skip its cancel.
    signedIn.unmount();
    queryClient.removeQueries({ queryKey: ["issues"] });
    const signedInAgain = renderProbe();
    await waitFor(() => expect(list.starts()).toBe(3));

    // An event during that load must restart it, so the body composed before the event never
    // reaches the screen.
    emit?.(issueUpdatedFrame(2));
    await waitFor(() => expect(list.starts()).toBe(4));
    // Load 3 was reverted, so its body can no longer reach the screen at all; the reader ends
    // on load 4, the one started after the event.
    list.release(3);
    list.release(4);
    await waitFor(() => expect(screen.getByText("list-4")).toBeTruthy());
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(screen.queryByText("list-3")).toBeNull();

    signedInAgain.unmount();
  } finally {
    list.releaseAll();
    globalThis.fetch = originalFetch;
  }
}, 20_000);

test("a settle refresh still fires for a key that was removed mid-load", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const list = deferredListQuery();
  let emit: ((frame: string) => void) | undefined;
  const renderProbe = () =>
    render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    const signedIn = renderProbe();
    await waitFor(() => expect(list.starts()).toBe(1));
    await waitFor(() => expect(emit).toBeDefined());

    // Load 1 is cancelled; load 2 is left to land and is owed a refresh when it settles. Then
    // the whole cache is dropped while load 2 is still in flight, which is what signing out
    // does - and `QueryCache.remove` cancels silently, so load 2 never reaches `"idle"`.
    emit?.(issueUpdatedFrame(1));
    await waitFor(() => expect(list.starts()).toBe(2));
    emit?.(issueUpdatedFrame(2));
    await new Promise((resolve) => setTimeout(resolve, 150));
    signedIn.unmount();
    queryClient.removeQueries({ queryKey: ["issues"] });
    list.release(2);
    await new Promise((resolve) => setTimeout(resolve, 150));

    // Signing back in. The refresh owed to the removed load must not be owed forever: this
    // key's next starved load still gets a settle refresh of its own.
    const signedInAgain = renderProbe();
    await waitFor(() => expect(list.starts()).toBe(3));
    emit?.(issueUpdatedFrame(3));
    await waitFor(() => expect(list.starts()).toBe(4));
    emit?.(issueUpdatedFrame(4));
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(list.starts()).toBe(4);

    list.release(4);
    await waitFor(() => expect(list.starts()).toBe(5));

    signedInAgain.unmount();
  } finally {
    list.releaseAll();
    globalThis.fetch = originalFetch;
  }
}, 20_000);

test("repeated visibility reconnects cost one whole-cache refresh plus a trailing one", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const invalidate = queryClient.invalidateQueries.bind(queryClient);
  let refreshes = 0;
  queryClient.invalidateQueries = ((filters?: { queryKey?: readonly unknown[] }) => {
    if (filters?.queryKey === undefined) {
      refreshes += 1;
    }
    return invalidate(filters);
  }) as typeof queryClient.invalidateQueries;
  const streamCalls: number[] = [];

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  try {
    globalThis.fetch = (async (
      _input: RequestInfo | URL,
      init?: RequestInit
    ): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    const { unmount } = renderHook(() => useEventStream(), { wrapper: Wrapper });
    await waitFor(() => expect(streamCalls.length).toBe(1));

    // `forceReconnect` resets the backoff on every `visibilitychange`, and a reconnect refresh
    // is unfiltered, so tab focus would otherwise refetch the GitHub proxy queries each time.
    for (let index = 0; index < 6; index += 1) {
      document.dispatchEvent(new Event("visibilitychange"));
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    await waitFor(() => expect(refreshes).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(refreshes).toBe(1);
    expect(streamCalls.length).toBeGreaterThan(2);

    // One trailing refresh carries whatever the later reopens would have asked for.
    await waitFor(() => expect(refreshes).toBe(2), { timeout: 8_000 });
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(refreshes).toBe(2);

    unmount();
  } finally {
    queryClient.invalidateQueries = invalidate;
    globalThis.fetch = originalFetch;
  }
}, 30_000);
