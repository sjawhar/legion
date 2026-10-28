import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, QueryObserver, useQuery } from "@tanstack/react-query";
import { type RenderResult, render, renderHook, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";

import { EVENT_REPLAY_HEADER, getConnectionState } from "../api/live";
import { DispatchQueryClient } from "../api/query-client";
import { useEventStream, WHOLE_CACHE_REFRESH_MS } from "../api/sse";
import { countWholeCacheRefreshes, openStreamResponse } from "./sse-test-harness";

function sleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
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

test("an out-of-order lower-id live event is applied and never regresses the reconnect cursor", async () => {
  const originalFetch = globalThis.fetch;
  const streamCalls: string[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  const frame = (id: number) =>
    `id: ${id}\nevent: issue.updated\ndata: {"id":${id},"issue_key":"CORE-1","seq":${id},"type":"issue.updated","actor":{"kind":"session","id":"s"},"notify":false,"created_at":"2026-01-01T00:00:00Z","payload":{}}\n\n`;

  let unmount: (() => void) | undefined;

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

    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;

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
  } finally {
    unmount?.();
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

  let unmount: (() => void) | undefined;

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

    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;

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
  } finally {
    unmount?.();
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

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (
      _input: RequestInfo | URL,
      init?: RequestInit
    ): Promise<Response> => {
      streamCalls.push(streamCalls.length);
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    unmount = render(
      <QueryClientProvider client={queryClient}>
        <Probe />
      </QueryClientProvider>
    ).unmount;

    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(fetches).toEqual({ checks: 1, issues: 1 }));
    // The page's first attempt opened, so there is nothing stale to refresh.
    const { promise: drained, resolve: drain } = Promise.withResolvers<void>();
    setTimeout(drain, 150);
    await drained;
    expect(fetches).toEqual({ checks: 1, issues: 1 });

    // A reconnect whose server says nothing of its replay may have missed events the stream never
    // saw, so every query the app holds refreshes - not only the ones some list remembered to
    // name - each within a second of its own last fetch.
    window.dispatchEvent(new Event("online"));
    await waitFor(() => expect(streamCalls.length).toBe(2));
    await waitFor(() => expect(fetches).toEqual({ checks: 2, issues: 2 }), { timeout: 2_000 });
  } finally {
    unmount?.();
    globalThis.fetch = originalFetch;
  }
}, 10_000);

// The page's queries are read when it loads, and a first attempt that never opened leaves the
// stream covering none of what followed; `forceReconnect` resets the backoff counter, so the
// `online` route reopens at attempt 0 like a first connection would.
const firstAttemptReopenCases = [
  ["the backoff timer", 2_000, () => {}],
  ["an online event", 500, () => window.dispatchEvent(new Event("online"))],
] as const;

test.each(
  firstAttemptReopenCases
)("the open after a first attempt that never opened refreshes everything (%s)", async (_reopen, timeout, reopen) => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const streamCalls: number[] = [];

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
        // What a network change does to a request: it rejects before any response.
        throw new TypeError("Failed to fetch");
      }
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;
    await waitFor(() => expect(streamCalls.length).toBe(1));
    await waitFor(() => expect(getConnectionState()).toBe("reconnecting"));
    reopen();
    await waitFor(() => expect(streamCalls.length).toBe(2), { timeout });
    await waitFor(() => expect(wholeCache.count()).toBe(1));
  } finally {
    unmount?.();
    wholeCache.restore();
    globalThis.fetch = originalFetch;
  }
});

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

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> =>
      openStreamResponse(init?.signal, "id: 1\nevent: future.event\ndata: {}\n\n")) as typeof fetch;

    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;
    const isStale = (key: readonly unknown[]) =>
      queryClient.getQueryCache().find({ queryKey: key, exact: true })?.isStale();

    await waitFor(() => expect(isStale(["issues"])).toBe(true));
    expect(isStale(["inbox"])).toBe(true);
    expect(isStale(["user-state"])).toBe(true);
    // A key no hand-written list ever named refreshes too.
    expect(isStale(["user-agent-state"])).toBe(true);
  } finally {
    unmount?.();
    globalThis.fetch = originalFetch;
  }
});

/**
 * A list query whose every load is released by the test, so a scenario can say exactly which
 * load is in flight when an event arrives. `starts` counts loads; `release(n)` answers load n.
 */
/** The list surface the deferred-load scenarios render: the stream plus one list query. */
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

test("unknown frames refresh at most once per window, leading and trailing", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // Nothing else in this test asks for a whole-cache refresh, so the count is exactly the
  // unknown-frame refreshes.
  const wholeCache = countWholeCacheRefreshes(queryClient);
  let emit: ((frame: string) => void) | undefined;

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;
    await waitFor(() => expect(emit).toBeDefined());

    // Fifty frames of a type this build cannot parse, inside one window. Unthrottled this is
    // fifty whole-cache refreshes, each of which reaches the GitHub proxy queries.
    for (let index = 1; index <= 50; index += 1) {
      emit?.(`id: ${index}\nevent: future.event\ndata: {}\n\n`);
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    await waitFor(() => expect(wholeCache.count()).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(wholeCache.count()).toBe(1);

    // One trailing refresh carries everything the burst asked for.
    await waitFor(() => expect(wholeCache.count()).toBe(2), { timeout: 8_000 });
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(wholeCache.count()).toBe(2);

    // And a frame after the window refreshes again: a second change of that same new type is
    // one this tab would otherwise never learn. This test overrides no window - it is the one
    // that runs at the production one, since its 50-frame burst is what bounds its runtime - so
    // it waits out the real constant rather than a copy of its value.
    await new Promise((resolve) => setTimeout(resolve, WHOLE_CACHE_REFRESH_MS));
    emit?.("id: 99\nevent: future.event\ndata: {}\n\n");
    await waitFor(() => expect(wholeCache.count()).toBe(3));
  } finally {
    unmount?.();
    wholeCache.restore();
    globalThis.fetch = originalFetch;
  }
}, 40_000);

test("repeated visibility reconnects cost one whole-cache refresh plus a trailing one", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const streamCalls: number[] = [];

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
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    // A short injected window, so the test waits out its own trailing refresh rather than the
    // production 5 s. The unknown-frames test keeps the real window: its burst is what bounds
    // its runtime, and shortening the window there without tightening the burst proves less.
    unmount = renderHook(() => useEventStream({ wholeCacheRefreshMs: 3_000 }), {
      wrapper: Wrapper,
    }).unmount;
    await waitFor(() => expect(streamCalls.length).toBe(1));

    // `forceReconnect` resets the backoff on every `visibilitychange`, and a reconnect refresh
    // is unfiltered, so tab focus would otherwise refetch the GitHub proxy queries each time.
    for (let index = 0; index < 6; index += 1) {
      document.dispatchEvent(new Event("visibilitychange"));
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    await waitFor(() => expect(wholeCache.count()).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(wholeCache.count()).toBe(1);
    expect(streamCalls.length).toBeGreaterThan(2);

    // One trailing refresh carries whatever the later reopens would have asked for.
    await waitFor(() => expect(wholeCache.count()).toBe(2), { timeout: 4_000 });
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(wholeCache.count()).toBe(2);
  } finally {
    unmount?.();
    wholeCache.restore();
    globalThis.fetch = originalFetch;
  }
}, 30_000);

test("a backwards system-clock step does not park the whole-cache throttle", async () => {
  const originalFetch = globalThis.fetch;
  const originalNow = Date.now;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  let emit: ((frame: string) => void) | undefined;

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    unmount = renderHook(() => useEventStream({ wholeCacheRefreshMs: 1_000 }), {
      wrapper: Wrapper,
    }).unmount;
    await waitFor(() => expect(emit).toBeDefined());

    emit?.("id: 1\nevent: future.event\ndata: {}\n\n");
    await waitFor(() => expect(wholeCache.count()).toBe(1));

    // An NTP correction, a daylight-saving change, or someone setting the clock: the wall clock
    // steps an hour backwards. Read on a wall clock the last refresh is then an hour in the
    // future and the next one is scheduled an hour out; on a monotonic clock the window is
    // unaffected and this second frame still refreshes.
    const stepped = originalNow() - 60 * 60 * 1_000;
    Date.now = () => stepped;

    emit?.("id: 2\nevent: future.event\ndata: {}\n\n");
    await waitFor(() => expect(wholeCache.count()).toBe(2), { timeout: 4_000 });
  } finally {
    unmount?.();
    Date.now = originalNow;
    wholeCache.restore();
    globalThis.fetch = originalFetch;
  }
}, 20_000);

test("an event during a list's first load keeps that load and fetches the list once more after it lands", async () => {
  const originalFetch = globalThis.fetch;
  // The production client's staleTime: a body that lands marked fresh would stay on screen for
  // half a minute, which is what makes skipping the second load a stale screen.
  const queryClient = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const list = deferredListQuery();
  let emit: ((frame: string) => void) | undefined;
  let view: RenderResult | undefined;

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    view = render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );
    const page = within(view.container);
    await waitFor(() => expect(list.starts()).toBe(1));
    await waitFor(() => expect(emit).toBeDefined());

    // The issue is renamed while the list's first request is still out. That request is the one
    // the page is waiting for: nothing cancels it, and no second request goes out beside it.
    emit?.(issueUpdatedFrame(1, "NEWTITLE"));
    await sleep(300);
    expect(list.starts()).toBe(1);

    list.release(1);
    await waitFor(() => expect(page.getByText("list-1")).toBeTruthy());
    // Its body may predate the rename, so exactly one more load follows, and the page ends on it.
    await waitFor(() => expect(list.starts()).toBe(2), { timeout: 2_000 });
    list.release(2);
    await waitFor(() => expect(page.getByText("list-2")).toBeTruthy());
    await sleep(1_200);
    expect(list.starts()).toBe(2);
  } finally {
    view?.unmount();
    list.releaseAll();
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("a child's status move refreshes its parent's detail and no other issue's", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const fetches: Record<string, number> = { "CORE-1": 0, "CORE-2": 0, "CORE-3": 0 };
  const unsubscribes = Object.keys(fetches).map((key) =>
    new QueryObserver(queryClient, {
      queryFn: async () => {
        fetches[key] += 1;
        return { key };
      },
      queryKey: ["issue", key],
    }).subscribe(() => {})
  );
  // CORE-2 is CORE-1's child; its own `issue.updated` names the ancestors further up.
  const childStatus = `id: 1\nevent: child.status\ndata: ${JSON.stringify({
    actor: { id: "s", kind: "session" },
    created_at: "2026-01-01T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: { child_key: "CORE-2", from: "todo", to: "in_progress" },
    project: "CORE",
    seq: 4,
    type: "child.status",
  })}\n\n`;

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    await waitFor(() => expect(fetches).toEqual({ "CORE-1": 1, "CORE-2": 1, "CORE-3": 1 }));
    globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> =>
      openStreamResponse(init?.signal, childStatus)) as typeof fetch;
    unmount = renderHook(() => useEventStream(), { wrapper: Wrapper }).unmount;

    await waitFor(() => expect(fetches["CORE-1"]).toBe(2), { timeout: 2_000 });
    await sleep(1_200);
    expect(fetches).toEqual({ "CORE-1": 2, "CORE-2": 1, "CORE-3": 1 });
  } finally {
    unmount?.();
    for (const unsubscribe of unsubscribes) {
      unsubscribe();
    }
    globalThis.fetch = originalFetch;
  }
}, 10_000);

test("a steady event trickle on a slow first load gives that load and one more after it settles", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const list = deferredListQuery();
  let emit: ((frame: string) => void) | undefined;
  let view: RenderResult | undefined;

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    view = render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );
    const page = within(view.container);
    await waitFor(() => expect(list.starts()).toBe(1));
    await waitFor(() => expect(emit).toBeDefined());

    // Events keep arriving, past the one-second mark, while the first load is still out: none of
    // them restarts it or sends a second request beside it.
    for (let index = 1; index <= 8; index += 1) {
      emit?.(issueUpdatedFrame(index));
      await sleep(150);
    }
    expect(list.starts()).toBe(1);

    // The held load lands and the reader leaves the skeleton.
    list.release(1);
    await waitFor(() => expect(page.getByText("list-1")).toBeTruthy());
    // It predates the later events, so exactly one more load starts, and the reader ends on it.
    await waitFor(() => expect(list.starts()).toBe(2), { timeout: 2_000 });
    list.release(2);
    await waitFor(() => expect(page.getByText("list-2")).toBeTruthy());
    await sleep(1_200);
    expect(list.starts()).toBe(2);
  } finally {
    view?.unmount();
    list.releaseAll();
    globalThis.fetch = originalFetch;
  }
}, 20_000);

test("a refresh owed to a list removed mid-load is not paid by its replacement, whose own events still refresh it", async () => {
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
  let view: RenderResult | undefined;

  try {
    globalThis.fetch = emittingStream((send) => {
      emit = send;
    });
    view = renderProbe();
    await waitFor(() => expect(list.starts()).toBe(1));
    await waitFor(() => expect(emit).toBeDefined());

    // Load 1 owes a refresh once it settles; then the whole cache is dropped while it is still in
    // flight, which is what signing out does.
    emit?.(issueUpdatedFrame(1));
    await sleep(150);
    view.unmount();
    queryClient.removeQueries({ queryKey: ["issues"] });
    list.release(1);
    await sleep(150);

    // Signing back in: the recreated list's first load owes nothing for the event before it.
    const signedInAgain = renderProbe();
    view = signedInAgain;
    await waitFor(() => expect(list.starts()).toBe(2));
    list.release(2);
    await waitFor(() => expect(within(signedInAgain.container).getByText("list-2")).toBeTruthy());
    await sleep(1_200);
    expect(list.starts()).toBe(2);

    // Its own next event refreshes it.
    emit?.(issueUpdatedFrame(2));
    await waitFor(() => expect(list.starts()).toBe(3), { timeout: 2_000 });
  } finally {
    view?.unmount();
    list.releaseAll();
    globalThis.fetch = originalFetch;
  }
}, 20_000);

test("a reconnect whose replay the server says is complete refreshes nothing, and one that says nothing refreshes everything", async () => {
  const originalFetch = globalThis.fetch;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const streamCalls: string[] = [];

  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  let unmount: (() => void) | undefined;

  try {
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      streamCalls.push(String(input));
      if (streamCalls.length === 1) {
        return openStreamResponse(init?.signal, issueUpdatedFrame(5));
      }
      if (streamCalls.length === 2) {
        // The resuming connection's cursor is still in the log: every event after it is replayed.
        return new Response(openStreamResponse(init?.signal).body, {
          headers: { [EVENT_REPLAY_HEADER]: "complete" },
          status: 200,
        });
      }
      // An older server, or a log reset under the cursor: nothing says what was missed.
      return openStreamResponse(init?.signal);
    }) as typeof fetch;

    unmount = renderHook(() => useEventStream({ wholeCacheRefreshMs: 100 }), {
      wrapper: Wrapper,
    }).unmount;
    await waitFor(() => expect(streamCalls.length).toBe(1));
    await sleep(150);

    window.dispatchEvent(new Event("online"));
    await waitFor(() => expect(streamCalls.length).toBe(2));
    expect(new URL(streamCalls[1], "http://localhost").searchParams.get("since")).toBe("5");
    await sleep(400);
    expect(wholeCache.count()).toBe(0);

    window.dispatchEvent(new Event("online"));
    await waitFor(() => expect(streamCalls.length).toBe(3));
    await waitFor(() => expect(wholeCache.count()).toBe(1));
  } finally {
    unmount?.();
    wholeCache.restore();
    globalThis.fetch = originalFetch;
  }
}, 10_000);
