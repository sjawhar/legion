import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, QueryObserver, useQuery } from "@tanstack/react-query";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../api/client";
import { useEventFeed, WHOLE_CACHE_REFRESH_MS } from "../api/event-feed";
import { liveSocketFor } from "../api/live-socket";
import type { AuthenticatedUser, Event, LiveServerFrame } from "../api/types";
import { credentialPendingQuery } from "../features/credentials/pending";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
} from "./live-socket-harness";

let sockets: FakeLiveSockets;
const clients: QueryClient[] = [];

beforeEach(() => {
  sockets = installFakeLiveSockets();
});

afterEach(() => {
  for (const client of clients.splice(0)) {
    liveSocketFor(client).close();
    client.clear();
  }
  sockets.restore();
});

/** A query client whose live socket this file's teardown closes. */
function newClient(queries: { staleTime?: number } = {}): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, ...queries } } });
  clients.push(client);
  return client;
}

function wrapperFor(client: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
}

/**
 * Counts whole-cache refreshes: `refreshQueries` is the only caller that invalidates with no key,
 * so an invalidation naming none is exactly one of them. Returns the count and a restore.
 */
function countWholeCacheRefreshes(queryClient: QueryClient): {
  count: () => number;
  restore: () => void;
} {
  const invalidate = queryClient.invalidateQueries.bind(queryClient);
  let refreshes = 0;
  queryClient.invalidateQueries = ((filters?: { queryKey?: readonly unknown[] }) => {
    if (filters?.queryKey === undefined) {
      refreshes += 1;
    }
    return invalidate(filters);
  }) as typeof queryClient.invalidateQueries;
  return {
    count: () => refreshes,
    restore: () => {
      queryClient.invalidateQueries = invalidate;
    },
  };
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

function issueUpdated(id: number, title = `title-${id}`): Event {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-23T00:00:00Z",
    id,
    issue_key: "CORE-1",
    notify: true,
    payload: { key: "CORE-1", title },
    project: "CORE",
    seq: id,
    type: "issue.updated",
  } as Event;
}

/** An event that refreshes nothing this page holds: another person's read state. */
function quietEvent(id: number): Event {
  return {
    actor: { id: "bob", kind: "user" },
    created_at: "2026-09-23T00:00:00Z",
    id,
    issue_key: null,
    notify: false,
    payload: { login: "bob" },
    seq: id,
    type: "user_state.updated",
  } as unknown as Event;
}

/** An event of a type this build does not know. */
function futureEvent(id: number): { readonly id: number } {
  return { id, type: "future.event", payload: {} } as { readonly id: number };
}

/** Dispatch's frame for `event` on the feed's subscription `sub`. */
function eventFrame(sub: string, event: { readonly id: number }): LiveServerFrame {
  return { type: "event", sub, id: String(event.id), event: event as Event };
}

function receive(socket: FakeLiveSocket, ...frames: LiveServerFrame[]): void {
  act(() => {
    for (const frame of frames) socket.receive(frame);
  });
}

/** Opens the socket the feed subscribed on, and Dispatch starts the feed after `since`. */
function openFeed(since = "0"): { socket: FakeLiveSocket; sub: string } {
  const socket = sockets.latest();
  act(() => socket.open());
  const { sub } = socket.subscribed("events");
  receive(socket, { type: "events_start", sub, since });
  return { socket, sub };
}

/** Dispatch rotates the socket (4001) and the page's next one opens: the feed's next send. */
function rotate(): FakeLiveSocket {
  act(() => sockets.latest().drop(4001));
  const next = sockets.latest();
  act(() => next.open());
  return next;
}

/** The sign-in check's answer as AuthGate holds it when the app mounts. */
function signIn(queryClient: QueryClient, eventHead?: string): void {
  const user: AuthenticatedUser =
    eventHead === undefined
      ? { kind: "user", login: "alice" }
      : { kind: "user", login: "alice", event_head: eventHead };
  queryClient.setQueryData(["whoami"], user);
}

test("a drop resumes from the cursor and refreshes everything", async () => {
  // No jitter below the base: the first retry waits 750 ms.
  const random = spyOn(Math, "random").mockReturnValue(0);
  const queryClient = newClient();
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const fetches = { checks: 0, issues: 0 };

  // An infinite `staleTime` keeps TanStack's own refetch-on-reconnect out of the result: these
  // queries are never stale by time, so any refetch below is the feed's doing. The checks key is
  // one no hand-maintained list of keys ever named - a pull request's check rows on an open issue.
  function Probe(): ReactNode {
    useEventFeed();
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

  const view = render(
    <QueryClientProvider client={queryClient}>
      <Probe />
    </QueryClientProvider>
  );
  try {
    await waitFor(() => expect(fetches).toEqual({ checks: 1, issues: 1 }));
    const { socket, sub } = openFeed("6");
    // The page's first subscription names no cursor, and refreshes nothing: its reads are its own.
    expect(socket.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
    });
    receive(socket, eventFrame(sub, quietEvent(7)));
    await sleep(150);
    expect(fetches).toEqual({ checks: 1, issues: 1 });
    expect(wholeCache.count()).toBe(0);

    act(() => socket.drop(1006));
    await waitFor(() => expect(sockets.sockets).toHaveLength(2), { timeout: 2_000 });
    const again = sockets.latest();
    act(() => again.open());
    expect(again.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
      resume: { since: "7" },
    });
    // While no socket carried the feed, events may have committed that its resume does not
    // replay, so every query the app holds refreshes - not only the ones some list names.
    await waitFor(() => expect(fetches).toEqual({ checks: 2, issues: 2 }));
    expect(wholeCache.count()).toBe(1);
  } finally {
    view.unmount();
    random.mockRestore();
    wholeCache.restore();
  }
});

test("a rotation resumes from the cursor and refreshes nothing", async () => {
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    const { socket, sub } = openFeed("5");
    receive(socket, eventFrame(sub, quietEvent(6)), eventFrame(sub, quietEvent(7)));

    const again = rotate();
    expect(sockets.sockets).toHaveLength(2);
    expect(again.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
      resume: { since: "7" },
    });
    await sleep(300);
    expect(wholeCache.count()).toBe(0);
  } finally {
    hook.unmount();
    wholeCache.restore();
  }
});

test("a page that has applied no event resumes from events_start after a rotation", () => {
  const queryClient = newClient();
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    const socket = sockets.latest();
    act(() => socket.open());
    const { sub } = socket.subscribed("events");
    expect(socket.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
    });
    receive(socket, { type: "events_start", sub, since: "120" });

    expect(rotate().subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
      resume: { since: "120" },
    });
  } finally {
    hook.unmount();
  }
});

test("an out-of-order lower id never lowers the cursor", () => {
  const queryClient = newClient();
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    const { socket, sub } = openFeed("99");
    // A higher id arrives first, then a lower one out of order - a transaction that committed
    // after a later one, which Dispatch forwards rather than filters by id.
    receive(socket, eventFrame(sub, quietEvent(101)), eventFrame(sub, quietEvent(100)));
    const again = rotate();
    expect(again.subscribed("events").resume).toEqual({ since: "101" });

    // A held cursor does not follow events_start either: a start Dispatch clamped to a lower
    // head leaves it where it was.
    receive(again, { type: "events_start", sub, since: "100" });
    expect(rotate().subscribed("events").resume).toEqual({ since: "101" });
  } finally {
    hook.unmount();
  }
});

test("an unknown event type refreshes everything, at most once per window", async () => {
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  for (const key of [["issues"], ["inbox"], ["user-state"], ["user-agent-state"]]) {
    queryClient.setQueryData(key, { loaded: true });
  }
  const isStale = (key: readonly unknown[]) =>
    queryClient.getQueryCache().find({ queryKey: key, exact: true })?.isStale();
  // Nothing else in this test asks for a whole-cache refresh, so the count is exactly the
  // unknown-event refreshes.
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    const { socket, sub } = openFeed();

    // The server may add an event this client cannot parse: rather than leave a rendered field
    // stale until the next gap, every query refreshes, a key no hand-written list named included.
    receive(socket, eventFrame(sub, futureEvent(1)));
    await waitFor(() => expect(isStale(["issues"])).toBe(true));
    expect(isStale(["inbox"])).toBe(true);
    expect(isStale(["user-state"])).toBe(true);
    expect(isStale(["user-agent-state"])).toBe(true);

    // Forty-nine more inside one window. Unthrottled this is fifty whole-cache refreshes, each of
    // which reaches the GitHub proxy queries.
    for (let index = 2; index <= 50; index += 1) {
      receive(socket, eventFrame(sub, futureEvent(index)));
      await sleep(20);
    }
    await waitFor(() => expect(wholeCache.count()).toBe(1));
    await sleep(500);
    expect(wholeCache.count()).toBe(1);

    // One trailing refresh carries everything the burst asked for.
    await waitFor(() => expect(wholeCache.count()).toBe(2), { timeout: 8_000 });
    await sleep(500);
    expect(wholeCache.count()).toBe(2);

    // And an event after the window refreshes again: a second change of that same new type is one
    // this tab would otherwise never learn. This test overrides no window - it is the one that
    // runs at the production one, since its 50-event burst is what bounds its runtime - so it
    // waits out the real constant rather than a copy of its value.
    await sleep(WHOLE_CACHE_REFRESH_MS);
    receive(socket, eventFrame(sub, futureEvent(99)));
    await waitFor(() => expect(wholeCache.count()).toBe(3));
  } finally {
    hook.unmount();
    wholeCache.restore();
  }
}, 40_000);

test("an event this page cannot read is skipped and refreshes everything", async () => {
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const logged = spyOn(console, "error").mockImplementation(() => {});
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    const { socket, sub } = openFeed();
    // A project event names its project; one that does not is not an event this page can map.
    const unreadable = { ...quietEvent(8), type: "project.created", payload: {} };
    receive(socket, eventFrame(sub, unreadable));
    await waitFor(() => expect(wholeCache.count()).toBe(1));
    expect(logged).toHaveBeenCalled();
    // The cursor moved past it before it was read, so no resume replays it.
    expect(rotate().subscribed("events").resume).toEqual({ since: "8" });
  } finally {
    hook.unmount();
    logged.mockRestore();
    wholeCache.restore();
  }
});

test("repeated gaps cost one refresh plus a trailing one", async () => {
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  // A short injected window, so the test waits out its own trailing refresh rather than the
  // production 5 s. The unknown-event test keeps the real window: its burst is what bounds its
  // runtime, and shortening the window there without tightening the burst proves less.
  const hook = renderHook(() => useEventFeed({ wholeCacheRefreshMs: 3_000 }), {
    wrapper: wrapperFor(queryClient),
  });
  try {
    openFeed();
    // Dispatch restarting node by node closes the socket again and again (1012), and every
    // socket after it reports a gap; each gap's refresh is unfiltered and reaches the GitHub
    // proxy queries.
    for (let index = 0; index < 6; index += 1) {
      act(() => sockets.latest().drop(1012));
      act(() => sockets.latest().open());
      await sleep(100);
    }
    expect(sockets.sockets).toHaveLength(7);
    await waitFor(() => expect(wholeCache.count()).toBe(1));
    await sleep(500);
    expect(wholeCache.count()).toBe(1);

    // One trailing refresh carries whatever the later gaps asked for.
    await waitFor(() => expect(wholeCache.count()).toBe(2), { timeout: 4_000 });
    await sleep(300);
    expect(wholeCache.count()).toBe(2);
  } finally {
    hook.unmount();
    wholeCache.restore();
  }
}, 30_000);

test("the credential list held null is asked again after a gap", async () => {
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const queryClient = newClient();
  let hook: { unmount: () => void } | undefined;
  try {
    // A Dispatch with no secrets broker answered the list with `null`, which no refresh asks again.
    await queryClient.prefetchQuery(credentialPendingQuery());
    expect(queryClient.getQueryData(credentialPendingQuery().queryKey)).toBeNull();
    hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
    openFeed();

    // A rotation is no gap, and asks nothing.
    rotate();
    await sleep(150);
    expect(getCredentialPending).toHaveBeenCalledTimes(1);

    // A gap is the one moment the server may have restarted, perhaps with a broker configured.
    act(() => sockets.latest().drop(1012));
    act(() => sockets.latest().open());
    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(2));
  } finally {
    hook?.unmount();
    getCredentialPending.mockRestore();
  }
});

// The page's queries are read when it loads, and a first handshake that failed leaves the feed
// covering none of what followed; `online` resets the backoff and reopens at once.
const firstOpenAfterFailure = [
  ["the backoff timer", () => {}],
  ["an online event", () => window.dispatchEvent(new Event("online"))],
] as const;

test.each(
  firstOpenAfterFailure
)("the open after a first handshake that failed refreshes everything (%s)", async (_reopen, reopen) => {
  const random = spyOn(Math, "random").mockReturnValue(0);
  const queryClient = newClient();
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    // What a network change does to a handshake: it fails before the socket opens.
    act(() => sockets.latest().fail());
    act(() => reopen());
    await waitFor(() => expect(sockets.sockets).toHaveLength(2), { timeout: 2_000 });
    act(() => sockets.latest().open());
    await waitFor(() => expect(wholeCache.count()).toBe(1));
  } finally {
    hook.unmount();
    random.mockRestore();
    wholeCache.restore();
  }
});

test("the first subscription resumes from the sign-in check's event head", async () => {
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  signIn(queryClient, "41");
  queryClient.setQueryData(["issue", "CORE-1"], { key: "CORE-1" });
  // A key no event below names: only a whole-cache refresh would reach it.
  const unnamed = ["github-link-checks", "https://github.com/sjawhar/legion/pull/1", "c0ffee"];
  queryClient.setQueryData(unnamed, { loaded: true });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    const socket = sockets.latest();
    act(() => socket.open());
    const { sub } = socket.subscribed("events");
    expect(socket.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
      resume: { since: "41" },
    });

    // Dispatch replays what committed since the sign-in check answered, which no read of the page
    // holds; each replayed event refreshes its own keys, and nothing else is read again.
    receive(
      socket,
      { type: "events_start", sub, since: "41" },
      eventFrame(sub, issueUpdated(42)),
      eventFrame(sub, issueUpdated(43))
    );
    await waitFor(() =>
      expect(queryClient.getQueryState(["issue", "CORE-1"])?.isInvalidated).toBe(true)
    );
    await sleep(150);
    expect(queryClient.getQueryState(unnamed)?.isInvalidated).toBe(false);
    expect(wholeCache.count()).toBe(0);
    expect(rotate().subscribed("events").resume).toEqual({ since: "43" });
  } finally {
    hook.unmount();
    wholeCache.restore();
  }
});

test("a later sign-in check does not move the start point", () => {
  const queryClient = newClient();
  signIn(queryClient, "41");
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    // The check is read again (a 1008 asks it) with a newer head before the first socket opens;
    // the page's reads were made after the first answer, so the feed starts there.
    act(() => signIn(queryClient, "90"));
    const socket = sockets.latest();
    act(() => socket.open());
    expect(socket.subscribed("events").resume).toEqual({ since: "41" });
  } finally {
    hook.unmount();
  }
});

test("with no event head the first subscription names no start point", () => {
  const queryClient = newClient();
  // A server older than `event_head`, during a deploy's overlap.
  signIn(queryClient);
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    const socket = sockets.latest();
    act(() => socket.open());
    const { sub } = socket.subscribed("events");
    expect(socket.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
    });
    // Dispatch starts the feed at its head, which the page adopts.
    receive(socket, { type: "events_start", sub, since: "77" });
    expect(rotate().subscribed("events").resume).toEqual({ since: "77" });
  } finally {
    hook.unmount();
  }
});

test("an event during a list's first load beats the response that predates it", async () => {
  // The production client's staleTime: a body that lands marked fresh stays on screen for half
  // a minute, which is what makes losing this race a stale screen rather than a blink.
  const queryClient = newClient({ staleTime: 30_000 });
  const titles = ["OLDTITLE", "NEWTITLE"];
  const { promise: held, resolve: release } = Promise.withResolvers<void>();
  let listCalls = 0;

  function Probe(): ReactNode {
    useEventFeed();
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

  const view = render(
    <QueryClientProvider client={queryClient}>
      <Probe />
    </QueryClientProvider>
  );
  try {
    // The list's first request is in flight and its response is held open.
    await waitFor(() => expect(listCalls).toBe(1));
    const { socket, sub } = openFeed();

    // The issue is renamed while that request is still out.
    receive(socket, eventFrame(sub, issueUpdated(1, "NEWTITLE")));

    // The invalidation issues a request of its own instead of waiting on the one already out.
    await waitFor(() => expect(listCalls).toBe(2));

    // Releasing the pre-event body must not put it back on screen, nor mark it fresh.
    release();
    await waitFor(() => expect(screen.getByText("NEWTITLE")).toBeTruthy());
    await sleep(200);
    expect(screen.getByText("NEWTITLE")).toBeTruthy();
  } finally {
    view.unmount();
  }
}, 10_000);

test("a queued prefix key replaces the narrower key instead of refetching it twice", async () => {
  const queryClient = newClient();
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
  let hook: { unmount: () => void } | undefined;

  try {
    await waitFor(() => expect(issueFetches).toBe(1));
    hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
    const { socket, sub } = openFeed();
    receive(
      socket,
      eventFrame(sub, {
        actor: { id: "s", kind: "session" },
        created_at: "2026-01-01T00:00:00Z",
        id: 1,
        issue_key: "CORE-1",
        notify: false,
        payload: { child_key: "CORE-2" },
        project: "CORE",
        seq: 4,
        type: "child.status",
      } as unknown as Event)
    );

    await waitFor(() => expect(issueFetches).toBe(2));
    // Without the prefix drop the cancelled refetch starts again and the count reaches 3.
    await sleep(400);
    expect(issueFetches).toBe(2);
  } finally {
    hook?.unmount();
    unsubscribe();
  }
}, 10_000);

interface DeferredListQuery {
  key: readonly unknown[];
  queryFn: () => Promise<string>;
  release: (load: number) => void;
  releaseAll: () => void;
  starts: () => number;
}

/**
 * A list query whose every load is released by the test, so a scenario can say exactly which
 * load is in flight when an event arrives. `starts` counts loads; `release(n)` answers load n.
 */
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

/** The list surface the deferred-load scenarios render: the feed plus one list query. */
function ListProbe({ list }: { list: DeferredListQuery }): ReactNode {
  useEventFeed();
  const { data } = useQuery({ queryFn: list.queryFn, queryKey: list.key });
  return <p>{data ?? "loading"}</p>;
}

test("a steady event trickle never starves a slow first load", async () => {
  const queryClient = newClient({ staleTime: 30_000 });
  const list = deferredListQuery();
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ListProbe list={list} />
    </QueryClientProvider>
  );

  try {
    await waitFor(() => expect(list.starts()).toBe(1));
    const { socket, sub } = openFeed();

    // The one cancel this query is allowed: load 1 is reverted and load 2 takes its place.
    receive(socket, eventFrame(sub, issueUpdated(1)));
    await waitFor(() => expect(list.starts()).toBe(2));

    // Every later event arrives while load 2 is still in flight. Cancelling on each of them is
    // what starved the reader; they register a settle refresh instead, and load 2 survives.
    for (let index = 2; index <= 5; index += 1) {
      receive(socket, eventFrame(sub, issueUpdated(index)));
      await sleep(150);
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
    await sleep(400);
    expect(list.starts()).toBe(3);
  } finally {
    view.unmount();
    list.releaseAll();
  }
}, 20_000);

test("a query removed and recreated between flushes still has its first load cancelled", async () => {
  const queryClient = newClient({ staleTime: 30_000 });
  const list = deferredListQuery();
  const renderProbe = () =>
    render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );
  const signedIn = renderProbe();
  let unmount = signedIn.unmount;

  try {
    await waitFor(() => expect(list.starts()).toBe(1));
    const { socket, sub } = openFeed();

    // Load 1 is cancelled, so this query's hash is recorded as already cancelled.
    receive(socket, eventFrame(sub, issueUpdated(1)));
    await waitFor(() => expect(list.starts()).toBe(2));

    // Sign out and straight back in, with no flush in between: the recreated query is a fresh
    // first load, and a stale hash would have it skip its cancel. The feed leaves with the first
    // render and subscribes again with the second, on a socket of its own.
    signedIn.unmount();
    queryClient.removeQueries({ queryKey: ["issues"] });
    const signedInAgain = renderProbe();
    unmount = signedInAgain.unmount;
    await waitFor(() => expect(list.starts()).toBe(3));
    await act(async () => {
      await Promise.resolve();
    });
    const again = openFeed();

    // An event during that load must restart it, so the body composed before the event never
    // reaches the screen.
    receive(again.socket, eventFrame(again.sub, issueUpdated(2)));
    await waitFor(() => expect(list.starts()).toBe(4));
    // Load 3 was reverted, so its body can no longer reach the screen at all; the reader ends
    // on load 4, the one started after the event.
    list.release(3);
    list.release(4);
    await waitFor(() => expect(screen.getByText("list-4")).toBeTruthy());
    await sleep(300);
    expect(screen.queryByText("list-3")).toBeNull();
  } finally {
    unmount();
    list.releaseAll();
  }
}, 20_000);

test("a settle refresh still fires for a key that was removed mid-load", async () => {
  const queryClient = newClient({ staleTime: 30_000 });
  const list = deferredListQuery();
  const renderProbe = () =>
    render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );
  const signedIn = renderProbe();
  let unmount = signedIn.unmount;

  try {
    await waitFor(() => expect(list.starts()).toBe(1));
    const { socket, sub } = openFeed();

    // Load 1 is cancelled; load 2 is left to land and is owed a refresh when it settles. Then
    // the whole cache is dropped while load 2 is still in flight, which is what signing out
    // does - and `QueryCache.remove` cancels silently, so load 2 never reaches `"idle"`.
    receive(socket, eventFrame(sub, issueUpdated(1)));
    await waitFor(() => expect(list.starts()).toBe(2));
    receive(socket, eventFrame(sub, issueUpdated(2)));
    await sleep(150);
    signedIn.unmount();
    queryClient.removeQueries({ queryKey: ["issues"] });
    list.release(2);
    await sleep(150);

    // Signing back in. The refresh owed to the removed load must not be owed forever: this
    // key's next starved load still gets a settle refresh of its own.
    const signedInAgain = renderProbe();
    unmount = signedInAgain.unmount;
    await waitFor(() => expect(list.starts()).toBe(3));
    await act(async () => {
      await Promise.resolve();
    });
    const again = openFeed();
    receive(again.socket, eventFrame(again.sub, issueUpdated(3)));
    await waitFor(() => expect(list.starts()).toBe(4));
    receive(again.socket, eventFrame(again.sub, issueUpdated(4)));
    await sleep(150);
    expect(list.starts()).toBe(4);

    list.release(4);
    await waitFor(() => expect(list.starts()).toBe(5));
  } finally {
    unmount();
    list.releaseAll();
  }
}, 20_000);

test("a backwards system-clock step does not park the whole-cache throttle", async () => {
  const originalNow = Date.now;
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const hook = renderHook(() => useEventFeed({ wholeCacheRefreshMs: 1_000 }), {
    wrapper: wrapperFor(queryClient),
  });

  try {
    const { socket, sub } = openFeed();
    receive(socket, eventFrame(sub, futureEvent(1)));
    await waitFor(() => expect(wholeCache.count()).toBe(1));

    // An NTP correction, a daylight-saving change, or someone setting the clock: the wall clock
    // steps an hour backwards. Read on a wall clock the last refresh is then an hour in the
    // future and the next one is scheduled an hour out; on a monotonic clock the window is
    // unaffected and this second event still refreshes.
    const stepped = originalNow() - 60 * 60 * 1_000;
    Date.now = () => stepped;

    receive(socket, eventFrame(sub, futureEvent(2)));
    await waitFor(() => expect(wholeCache.count()).toBe(2), { timeout: 4_000 });
  } finally {
    hook.unmount();
    Date.now = originalNow;
    wholeCache.restore();
  }
}, 20_000);
