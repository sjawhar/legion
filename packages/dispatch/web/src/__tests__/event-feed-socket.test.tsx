import { afterEach, beforeEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, QueryObserver, useQuery } from "@tanstack/react-query";
import {
  act,
  type RenderResult,
  render,
  renderHook,
  waitFor,
  within,
} from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../api/client";
import { useEventFeed, WHOLE_CACHE_REFRESH_MS } from "../api/event-feed";
import { liveSocketFor } from "../api/live-socket";
import { DispatchQueryClient } from "../api/query-client";
import type { AuthenticatedUser, Event, LiveServerFrame } from "../api/types";
import { credentialPendingQuery } from "../features/credentials/pending";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  RESTART_RECONNECT_MS,
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

/** A query client whose live socket this file's teardown closes: TanStack's, or the production
 *  `DispatchQueryClient` where a test reads the never-cancel refetch it adds. */
function newClient(
  queries: { staleTime?: number } = {},
  Client: typeof QueryClient = QueryClient
): QueryClient {
  const client = new Client({ defaultOptions: { queries: { retry: false, ...queries } } });
  clients.push(client);
  return client;
}

function wrapperFor(client: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
}

const WHOLE_CACHE_PROBE = ["whole-cache-refresh-probe"] as const;

/**
 * Counts whole-cache refreshes: a refresh that names no target marks every query the app holds
 * stale, and no event names this probe's key, so each time the probe goes stale is one of them.
 * The probe is made fresh again at once, ready for the next. Returns the count and a restore.
 */
function countWholeCacheRefreshes(queryClient: QueryClient): {
  count: () => number;
  restore: () => void;
} {
  let refreshes = 0;
  queryClient.setQueryData(WHOLE_CACHE_PROBE, refreshes);
  const unsubscribe = queryClient.getQueryCache().subscribe((change) => {
    if (
      change.type === "updated" &&
      change.action.type === "invalidate" &&
      change.query.queryKey[0] === WHOLE_CACHE_PROBE[0]
    ) {
      refreshes += 1;
      queryClient.setQueryData(WHOLE_CACHE_PROBE, refreshes);
    }
  });
  return {
    count: () => refreshes,
    restore: () => {
      unsubscribe();
      queryClient.removeQueries({ exact: true, queryKey: WHOLE_CACHE_PROBE });
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

/** Opens the socket the feed subscribed on, and Dispatch starts the feed after `since`, saying its
 *  replay covers the gap only with `replayComplete`. Without it, a start after a gap is one whose
 *  replay may have missed events - a resume above a reset log's head, or none at all. */
function openFeed(since = "0", replayComplete = false): { socket: FakeLiveSocket; sub: string } {
  const socket = sockets.latest();
  act(() => socket.open());
  const { sub } = socket.subscribed("events");
  receive(socket, { type: "events_start", sub, since, replay_complete: replayComplete });
  return { socket, sub };
}

/** Dispatch rotates the socket (4001) and the page's next one opens: the feed's next send. */
function rotate(): FakeLiveSocket {
  act(() => sockets.latest().drop(4001));
  const next = sockets.latest();
  act(() => next.open());
  return next;
}

/** The sign-in check's answer as AuthGate holds it when the app mounts. Every answer carries an
 *  event head; `"0"` is an empty log, which the feed resumes from as it does any other. */
function signIn(queryClient: QueryClient, eventHead = "0"): void {
  const user: AuthenticatedUser = { event_head: eventHead, kind: "user", login: "alice" };
  queryClient.setQueryData(["whoami"], user);
}

test("a drop resumes from the cursor and, when the replay misses the gap, refreshes everything", async () => {
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
    // While no socket carried the feed, Dispatch's log was reset: the resume is above its head, so
    // Dispatch starts at the head and replays nothing the page missed. Once the feed starts again
    // every query the app holds refreshes - not only the ones some list names - each within a
    // second of its own last fetch.
    receive(again, { type: "events_start", sub, since: "3", replay_complete: false });
    await waitFor(() => expect(fetches).toEqual({ checks: 2, issues: 2 }), { timeout: 2_000 });
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
    receive(socket, { type: "events_start", sub, since: "120", replay_complete: false });

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
    receive(again, { type: "events_start", sub, since: "100", replay_complete: false });
    expect(rotate().subscribed("events").resume).toEqual({ since: "101" });
  } finally {
    hook.unmount();
  }
});

/** Moves the fake clock `ms` on in steps, letting each timer's continuation and React's updates
 *  run as it fires. */
async function advance(ms: number): Promise<void> {
  for (let left = ms; left > 0; left -= 50) {
    await act(async () => {
      jest.advanceTimersByTime(Math.min(50, left));
      for (let turn = 0; turn < 10; turn += 1) await Promise.resolve();
    });
  }
}

// On the fake clock, at the production window: the burst, the trailing refresh and the event
// after the window are each read at the moment they are due, not waited out for real.
test("an unknown event type refreshes everything, at most once per window", async () => {
  jest.useFakeTimers();
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
    await advance(200);
    expect(isStale(["issues"])).toBe(true);
    expect(isStale(["inbox"])).toBe(true);
    expect(isStale(["user-state"])).toBe(true);
    expect(isStale(["user-agent-state"])).toBe(true);

    // Forty-nine more inside one window. Unthrottled this is fifty whole-cache refreshes, each of
    // which reaches the GitHub proxy queries.
    for (let index = 2; index <= 50; index += 1) {
      receive(socket, eventFrame(sub, futureEvent(index)));
      await advance(20);
    }
    await advance(500);
    expect(wholeCache.count()).toBe(1);

    // One trailing refresh carries everything the burst asked for, once the window is out.
    await advance(WHOLE_CACHE_REFRESH_MS);
    expect(wholeCache.count()).toBe(2);
    await advance(500);
    expect(wholeCache.count()).toBe(2);

    // And an event after the window refreshes again: a second change of that same new type is one
    // this tab would otherwise never learn.
    await advance(WHOLE_CACHE_REFRESH_MS);
    receive(socket, eventFrame(sub, futureEvent(99)));
    await advance(200);
    expect(wholeCache.count()).toBe(3);
  } finally {
    hook.unmount();
    wholeCache.restore();
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
});

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
  jest.useFakeTimers();
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
  try {
    openFeed();
    // Dispatch restarting node by node closes the socket again and again (1012), and every
    // socket after it reports a gap whose replay Dispatch does not say is complete (`openFeed`);
    // each gap's refresh is unfiltered and reaches the GitHub proxy queries.
    for (let index = 0; index < 6; index += 1) {
      act(() => sockets.latest().restart());
      await advance(RESTART_RECONNECT_MS);
      openFeed();
      await advance(100 - RESTART_RECONNECT_MS);
    }
    expect(sockets.sockets).toHaveLength(7);
    await advance(500);
    expect(wholeCache.count()).toBe(1);

    // One trailing refresh carries whatever the later gaps asked for, once the window is out.
    await advance(WHOLE_CACHE_REFRESH_MS);
    expect(wholeCache.count()).toBe(2);
    await advance(500);
    expect(wholeCache.count()).toBe(2);
  } finally {
    hook.unmount();
    wholeCache.restore();
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
});

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

    // A gap is the one moment the server may have restarted, perhaps with a broker configured. A
    // replay that covers the gap says nothing of that, so the list is asked again even then.
    const before = sockets.sockets.length;
    act(() => sockets.latest().restart());
    await waitFor(() => expect(sockets.sockets).toHaveLength(before + 1));
    openFeed("0", true);
    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(2));
  } finally {
    hook?.unmount();
    getCredentialPending.mockRestore();
  }
});

// While Dispatch cannot start the feed - its database does not answer - it ends each subscription
// a second after it is sent. A refresh at each send would refetch every query of every tab for as
// long as the outage lasts; the page refreshes once, when the feed starts again with a replay that
// does not cover the gap (one that does refreshes nothing, as any complete resume does).
test("ended frames with no events_start back off and cost one refresh once the feed starts", () => {
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  // A window shorter than every backoff, so a refresh at each send would show as one each.
  const hook = renderHook(() => useEventFeed({ wholeCacheRefreshMs: 100 }), {
    wrapper: wrapperFor(queryClient),
  });
  const ended = (sub: string) => ({ type: "ended", sub, reason: "behind" }) as const;
  try {
    const { socket, sub } = openFeed("30");
    receive(socket, ended(sub));
    expect(socket.subscribes()).toHaveLength(2);
    for (const [index, wait] of [1_000, 2_000, 4_000].entries()) {
      receive(socket, ended(sub));
      act(() => jest.advanceTimersByTime(wait - 1));
      expect(socket.subscribes()).toHaveLength(2 + index);
      act(() => jest.advanceTimersByTime(1));
      expect(socket.subscribes()).toHaveLength(3 + index);
    }
    act(() => jest.advanceTimersByTime(1_000));
    expect(wholeCache.count()).toBe(0);

    receive(socket, { type: "events_start", sub, since: "30", replay_complete: false });
    act(() => jest.advanceTimersByTime(1_000));
    expect(wholeCache.count()).toBe(1);
  } finally {
    // A zero-delay timer left on the fake clock would hang the next test's waitFor.
    act(() => jest.advanceTimersByTime(1));
    jest.useRealTimers();
    hook.unmount();
    random.mockRestore();
    wholeCache.restore();
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
    openFeed();
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
      { type: "events_start", sub, since: "41", replay_complete: true },
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

// W14. Every answer carries a head, so there is no page that subscribes without one: an empty
// log answers "0", which the feed resumes from, and Dispatch replays the whole log to it.
test("an empty log's head is resumed from like any other", () => {
  const queryClient = newClient();
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
      resume: { since: "0" },
    });
    // A start Dispatch clamped to its own head leaves the cursor where the page put it.
    receive(socket, { type: "events_start", sub, since: "0", replay_complete: true });
    expect(rotate().subscribed("events").resume).toEqual({ since: "0" });
  } finally {
    hook.unmount();
  }
});

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

test("an event during a list's first load keeps that load and fetches the list once more after it lands", async () => {
  // The production client's staleTime: a body that lands marked fresh would stay on screen for
  // half a minute, which is what makes skipping the second load a stale screen.
  const queryClient = newClient({ staleTime: 30_000 }, DispatchQueryClient);
  const list = deferredListQuery();
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ListProbe list={list} />
    </QueryClientProvider>
  );
  try {
    const page = within(view.container);
    await waitFor(() => expect(list.starts()).toBe(1));
    const { socket, sub } = openFeed();

    // The issue is renamed while the list's first request is still out. That request is the one
    // the page is waiting for: nothing cancels it, and no second request goes out beside it.
    receive(socket, eventFrame(sub, issueUpdated(1, "NEWTITLE")));
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
    view.unmount();
    list.releaseAll();
  }
}, 10_000);

test("a child's status move refreshes its parent's detail and no other issue's", async () => {
  const queryClient = newClient();
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
  let hook: { unmount: () => void } | undefined;

  try {
    await waitFor(() => expect(fetches).toEqual({ "CORE-1": 1, "CORE-2": 1, "CORE-3": 1 }));
    hook = renderHook(() => useEventFeed(), { wrapper: wrapperFor(queryClient) });
    const { socket, sub } = openFeed();
    // CORE-2 is CORE-1's child; its own `issue.updated` names the ancestors further up.
    receive(
      socket,
      eventFrame(sub, {
        actor: { id: "s", kind: "session" },
        created_at: "2026-01-01T00:00:00Z",
        id: 1,
        issue_key: "CORE-1",
        notify: false,
        payload: { child_key: "CORE-2", from: "todo", to: "in_progress" },
        project: "CORE",
        seq: 4,
        type: "child.status",
      } as unknown as Event)
    );

    await waitFor(() => expect(fetches["CORE-1"]).toBe(2), { timeout: 2_000 });
    await sleep(1_200);
    expect(fetches).toEqual({ "CORE-1": 2, "CORE-2": 1, "CORE-3": 1 });
  } finally {
    hook?.unmount();
    for (const unsubscribe of unsubscribes) {
      unsubscribe();
    }
  }
}, 10_000);

test("a steady event trickle on a slow first load gives that load and one more after it settles", async () => {
  const queryClient = newClient({ staleTime: 30_000 }, DispatchQueryClient);
  const list = deferredListQuery();
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ListProbe list={list} />
    </QueryClientProvider>
  );

  try {
    const page = within(view.container);
    await waitFor(() => expect(list.starts()).toBe(1));
    const { socket, sub } = openFeed();

    // Events keep arriving, past the one-second mark, while the first load is still out: none of
    // them restarts it or sends a second request beside it.
    for (let index = 1; index <= 8; index += 1) {
      receive(socket, eventFrame(sub, issueUpdated(index)));
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
    view.unmount();
    list.releaseAll();
  }
}, 20_000);

test("a refresh owed to a list removed mid-load is not paid by its replacement, whose own events still refresh it", async () => {
  const queryClient = newClient({ staleTime: 30_000 });
  const list = deferredListQuery();
  const renderProbe = () =>
    render(
      <QueryClientProvider client={queryClient}>
        <ListProbe list={list} />
      </QueryClientProvider>
    );
  let view: RenderResult = renderProbe();

  try {
    await waitFor(() => expect(list.starts()).toBe(1));
    const { socket, sub } = openFeed();

    // Load 1 owes a refresh once it settles; then the whole cache is dropped while it is still in
    // flight, which is what signing out does.
    receive(socket, eventFrame(sub, issueUpdated(1)));
    await sleep(150);
    view.unmount();
    queryClient.removeQueries({ queryKey: ["issues"] });
    list.release(1);
    await sleep(150);

    // Signing back in: the recreated list's first load owes nothing for the event before it. The
    // feed left with the first render and subscribes again with the second, on a socket of its
    // own.
    const signedInAgain = renderProbe();
    view = signedInAgain;
    await waitFor(() => expect(list.starts()).toBe(2));
    await act(async () => {
      await Promise.resolve();
    });
    const again = openFeed();
    list.release(2);
    await waitFor(() => expect(within(signedInAgain.container).getByText("list-2")).toBeTruthy());
    await sleep(1_200);
    expect(list.starts()).toBe(2);

    // Its own next event refreshes it.
    receive(again.socket, eventFrame(again.sub, issueUpdated(2)));
    await waitFor(() => expect(list.starts()).toBe(3), { timeout: 2_000 });
  } finally {
    view.unmount();
    list.releaseAll();
  }
}, 20_000);

// A resume Dispatch replays in full (`replay_complete`: the resume is at or below its head) missed
// nothing, so the start refreshes nothing; each replayed event refreshes what it changed. A start
// whose replay does not cover the gap refreshes everything, as before.
test("a resume whose replay Dispatch says is complete refreshes nothing, and one that is not refreshes everything", async () => {
  const queryClient = newClient({ staleTime: Number.POSITIVE_INFINITY });
  const wholeCache = countWholeCacheRefreshes(queryClient);
  const hook = renderHook(() => useEventFeed({ wholeCacheRefreshMs: 100 }), {
    wrapper: wrapperFor(queryClient),
  });
  try {
    const { socket, sub } = openFeed("4");
    receive(socket, eventFrame(sub, issueUpdated(5)));
    await sleep(150);

    // The socket is lost, and the page resumes after the event it applied: Dispatch's log still
    // holds it, so every event after it is replayed.
    act(() => socket.restart());
    await waitFor(() => expect(sockets.latest()).not.toBe(socket));
    const again = sockets.latest();
    act(() => again.open());
    expect(again.subscribed("events").resume).toEqual({ since: "5" });
    receive(again, { type: "events_start", sub, since: "5", replay_complete: true });
    await sleep(400);
    expect(wholeCache.count()).toBe(0);

    // Lost again, and the log was reset under the cursor: Dispatch starts at its new head and
    // says the replay does not cover the gap.
    act(() => again.restart());
    await waitFor(() => expect(sockets.latest()).not.toBe(again));
    const third = sockets.latest();
    act(() => third.open());
    receive(third, { type: "events_start", sub, since: "2", replay_complete: false });
    await waitFor(() => expect(wholeCache.count()).toBe(1));
  } finally {
    hook.unmount();
    wholeCache.restore();
  }
}, 10_000);

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
