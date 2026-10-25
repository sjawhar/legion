import { afterEach, beforeEach, expect, jest, spyOn, test } from "bun:test";
import type { AgentStreamFrame } from "@legion/contracts";
import { QueryClient, QueryClientProvider, QueryObserver } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
} from "../__tests__/live-socket-harness";
import { ApiError, isDeliveryNotConfigured } from "./client";
import {
  type AgentHandlers,
  createLiveSocket,
  type EventsHandlers,
  LIVE_SOCKET_STUCK_MS,
  type LiveSocket,
  type LiveSocketState,
  liveSocketFor,
  reconnectDelayMs,
  useLiveSocketPill,
} from "./live-socket";
import { refreshQueries } from "./query-refresh";
import type {
  DeliveryLiveBody,
  DeliveryLiveDelta,
  DeliveryLiveRevs,
  DeliveryLiveSnapshot,
  DeliveryLiveView,
  DeliveryPR,
  DeliveryTimelineResponse,
  LiveEventFrame,
} from "./types";

const view: DeliveryLiveView = { kind: "timeline", window: { preset: 28 }, query: "" };
const key = ["delivery", "timeline", view] as const;

const emptyCounts = {
  repo: {},
  parent_agent: {},
  session: {},
  issue: {},
  priority: {},
  component: {},
  author: {},
  rework: {},
  deployed: {},
};

const pr: DeliveryPR = {
  additions: null,
  author: "octocat",
  components: [],
  created_at: "2026-10-01T01:00:00Z",
  deletions: null,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2026-10-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: one",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

const body = {
  window: { from: "2026-09-03T00:00:00Z", to: "2026-10-01T00:00:00Z" },
  prs: [],
  waiting: [],
  runs: [],
  facet_counts: emptyCounts,
  color_counts: { repo: {}, author: {}, priority: {}, component: {}, parent_agent: {} },
  components: {},
  issue_titles: {},
  freshness: {
    last_event_at: null,
    last_reconcile_at: null,
    last_error: null,
    unfetchable_count: 0,
  },
  measures: {},
  session_groups: [],
  session_titles: {},
  session_pr_counts: {},
} as unknown as DeliveryTimelineResponse;

const revs: DeliveryLiveRevs = {
  items: { prs: {}, runs: {}, session_groups: {} },
  maps: {},
  counts: {},
  fields: { window: "0000000000000001" },
};

function snapshot(sub: string, seq = "40", changedAt: string | null = "2026-10-01T00:00:00Z") {
  return {
    type: "snapshot",
    sub,
    seq,
    changed_at: changedAt,
    body,
    revs,
  } satisfies DeliveryLiveSnapshot;
}

function merged(sub: string, seq = "41"): DeliveryLiveDelta {
  return {
    type: "delta",
    sub,
    seq,
    changed_at: "2026-10-01T00:01:00Z",
    set: {},
    items: { prs: { upsert: [pr], remove: [] } },
    maps: {},
    counts: { facet_counts: { repo: { "acme/widgets": 1 } } },
    revs: {
      items: { prs: { "acme/widgets#1": "aaaaaaaaaaaaaaaa" } },
      counts: { facet_counts: { repo: { "acme/widgets": "bbbbbbbbbbbbbbbb" } } },
    },
  };
}

const SESSION_A = "11111111-1111-4111-8111-111111111111";
const SESSION_B = "22222222-2222-4222-8222-222222222222";

let sockets: FakeLiveSockets;
let queryClient: QueryClient;
let live: LiveSocket;

beforeEach(() => {
  sockets = installFakeLiveSockets();
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  live = createLiveSocket(queryClient);
});

afterEach(() => {
  live.close();
  queryClient.clear();
  sockets.restore();
  jest.useRealTimers();
});

/** The page's live query options, without subscribing an observer. */
function liveObserver(liveView = view, queryKey: readonly unknown[] = key) {
  return new QueryObserver(queryClient, {
    queryKey,
    queryFn: ({ queryKey: k }) => live.subscribe(k, liveView),
    meta: { live: true },
    staleTime: Number.POSITIVE_INFINITY,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    retry: false,
  });
}

/** A live query as the page reads it: queryFn subscribes, the result comes from the cache. */
function observe(liveView = view, queryKey: readonly unknown[] = key) {
  const observer = liveObserver(liveView, queryKey);
  const unsubscribe = observer.subscribe(() => {});
  return { observer, unsubscribe };
}

/** Lets the socket's pending callbacks and the query's promise chain run. */
async function settle() {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
}

/** The socket opened and the subscribe frame it sent. */
async function opened(): Promise<{ socket: FakeLiveSocket; sub: string }> {
  await settle();
  const socket = sockets.latest();
  socket.open();
  const sub = socket.subscribed("timeline").sub;
  return { socket, sub };
}

test("the snapshot is written with setQueryData and resolves the first load", async () => {
  const { observer, unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    expect(socket.url).toBe(`${protocol}//${window.location.host}/ws/live`);
    expect(socket.subscribed("timeline")).toEqual({ type: "subscribe", sub, view });

    socket.receive(snapshot(sub));
    await settle();
    expect(observer.getCurrentResult().status).toBe("success");
    expect(queryClient.getQueryData<DeliveryTimelineResponse>(key)).toEqual(body);
    expect(queryClient.getQueryState(key)?.fetchStatus).toBe("idle");
  } finally {
    unsubscribe();
  }
});

test("a delta updates the cached data and nothing refetches", async () => {
  const { unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));
    await settle();

    const before = queryClient.getQueryData<DeliveryTimelineResponse>(key);
    const fetches = queryClient.getQueryState(key)?.dataUpdateCount;
    socket.receive(merged(sub));
    await settle();
    const data = queryClient.getQueryData<DeliveryTimelineResponse>(key);
    expect(data?.prs).toEqual([pr]);
    expect(data?.facet_counts.repo).toEqual({ "acme/widgets": 1 });
    // A field the delta did not name is the very object the page already held.
    expect(data?.window).toBe(before?.window);
    expect(queryClient.getQueryState(key)?.dataUpdateCount).toBe((fetches ?? 0) + 1);
    expect(queryClient.getQueryState(key)?.fetchStatus).toBe("idle");
    // One subscribe, sent once: the delta re-read nothing.
    expect(socket.subscribes()).toHaveLength(1);
  } finally {
    unsubscribe();
  }
});

test("a snapshot lands in the cache even when the first load was cancelled", async () => {
  const { observer, unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    await queryClient.cancelQueries({ queryKey: key });
    expect(queryClient.getQueryState(key)?.fetchStatus).toBe("idle");

    socket.receive(snapshot(sub));
    await settle();
    expect(queryClient.getQueryData<DeliveryTimelineResponse>(key)).toEqual(body);
    expect(observer.getCurrentResult().status).toBe("success");
  } finally {
    unsubscribe();
  }
});

test("a query collected before its observer subscribed still gets its snapshot", async () => {
  // React builds the page's query while it renders and subscribes its observer in a later effect.
  // gcTime 0 can collect that first query in between; subscribing then builds a second query under
  // the same hash, and the first one's own removal notice arrives after the second subscribed.
  // Each collection is the cache's remove, called here as the gcTime 0 timer calls it: happy-dom
  // groups zero-delay timeouts into one batch an earlier test can leave pending, so a timer would
  // collect nothing in some orders.
  const cache = queryClient.getQueryCache();
  const observer = liveObserver();
  const first = observer.getCurrentQuery();
  cache.remove(first);
  expect(cache.get(first.queryHash)).toBeUndefined();
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    const socket = sockets.latest();
    const second = observer.getCurrentQuery();
    expect(second).not.toBe(first);
    expect(cache.get<DeliveryLiveBody>(first.queryHash)).toBe(second);
    cache.remove(first);
    await settle();
    expect(socket.closedWith).toBeUndefined();
    socket.open();
    socket.receive(snapshot(socket.subscribed("timeline").sub));
    await settle();
    expect(observer.getCurrentResult().status).toBe("success");
    expect(queryClient.getQueryData<DeliveryTimelineResponse>(key)).toEqual(body);
  } finally {
    unsubscribe();
  }
});

test("a drop reconnects with backoff and resumes with seq and revs", async () => {
  jest.useFakeTimers();
  const { unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));
    socket.receive(merged(sub, "41"));
    await settle();
    expect(live.state().state).toBe("open");

    socket.drop(1006);
    expect(live.state().state).toBe("reconnecting");
    expect(sockets.sockets).toHaveLength(1);
    // The first attempt waits a second, give or take its jitter, then opens a new socket.
    jest.advanceTimersByTime(499);
    expect(sockets.sockets).toHaveLength(1);
    jest.advanceTimersByTime(1_200);
    expect(sockets.sockets).toHaveLength(2);

    const again = sockets.latest();
    again.open();
    expect(again.subscribed("timeline")).toEqual({
      type: "subscribe",
      sub,
      view,
      resume: {
        seq: "41",
        revs: {
          ...revs,
          items: { ...revs.items, prs: { "acme/widgets#1": "aaaaaaaaaaaaaaaa" } },
          counts: { facet_counts: { repo: { "acme/widgets": "bbbbbbbbbbbbbbbb" } } },
        },
      },
    });

    // A second drop before anything arrived waits twice as long.
    again.drop(1006);
    jest.advanceTimersByTime(1_400);
    expect(sockets.sockets).toHaveLength(2);
    jest.advanceTimersByTime(1_200);
    expect(sockets.sockets).toHaveLength(3);
  } finally {
    unsubscribe();
  }
});

test("a connection error before open reconnects with backoff", async () => {
  jest.useFakeTimers();
  const { unsubscribe } = observe();
  try {
    await settle();
    sockets.latest().fail();

    expect(live.state().state).toBe("reconnecting");
    // The first attempt waits a second, give or take a quarter for its jitter: 750 to 1,250 ms.
    jest.advanceTimersByTime(749);
    expect(sockets.sockets).toHaveLength(1);
    jest.advanceTimersByTime(502);
    expect(sockets.sockets).toHaveLength(2);
  } finally {
    unsubscribe();
  }
});

test("close 1012 reconnects at once", async () => {
  jest.useFakeTimers();
  const { unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));
    socket.drop(1012);
    // No backoff timer: the new socket is already opening.
    expect(sockets.sockets).toHaveLength(2);
  } finally {
    unsubscribe();
  }
});

test("an error frame rejects the query with an ApiError carrying the code", async () => {
  const { observer, unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    socket.receive({
      type: "error",
      sub,
      code: "DELIVERY_NOT_CONFIGURED",
      message: "delivery is not configured",
    });
    await settle();
    const result = observer.getCurrentResult();
    expect(result.status).toBe("error");
    expect(result.error).toBeInstanceOf(ApiError);
    expect(isDeliveryNotConfigured(result.error)).toBe(true);
    expect((result.error as ApiError).message).toBe("delivery is not configured");

    // Once configured, the page's refetch subscribes again and the snapshot lands.
    void observer.refetch();
    await settle();
    const retried = socket.subscribes().at(-1);
    expect(retried?.sub).not.toBe(sub);
    if (retried === undefined) throw new Error("no new subscription");
    socket.receive(snapshot(retried.sub));
    await settle();
    expect(observer.getCurrentResult().status).toBe("success");
  } finally {
    unsubscribe();
  }
});

test("the last observer leaving unsubscribes", async () => {
  const first = observe();
  const second = observe();
  const { socket, sub } = await opened();
  socket.receive(snapshot(sub));
  await settle();
  expect(socket.subscribes()).toHaveLength(1);

  first.unsubscribe();
  expect(socket.sent.filter((frame) => frame.type === "unsubscribe")).toHaveLength(0);
  second.unsubscribe();
  expect(socket.sent.at(-1)).toEqual({ type: "unsubscribe", sub });
  // A frame for it still in flight writes nothing.
  socket.receive(merged(sub));
  expect(queryClient.getQueryData<DeliveryTimelineResponse>(key)?.prs).toEqual([]);
  // With nothing subscribed the socket closes, once nothing subscribes again within the task.
  await settle();
  expect(socket.closedWith).toBe(1000);
});

test("a socket silent past three heartbeats is replaced when the tab becomes visible", async () => {
  jest.useFakeTimers();
  const start = Date.now();
  jest.setSystemTime(start);
  const { unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));

    jest.setSystemTime(start + 44_000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(sockets.sockets).toHaveLength(1);
    expect(socket.closedWith).toBeUndefined();

    jest.setSystemTime(start + 46_000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(socket.closedWith).toBe(4000);
    jest.advanceTimersByTime(0);
    expect(sockets.sockets).toHaveLength(2);
    sockets.latest().open();
    expect(sockets.latest().subscribed("timeline").resume).toMatchObject({ seq: "40" });
  } finally {
    unsubscribe();
  }
});

test("heartbeats keep a quiet socket from being replaced, and 46 s with no frame replaces it", async () => {
  jest.useFakeTimers();
  const start = Date.now();
  jest.setSystemTime(start);
  const { unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));

    // Nothing changes for a minute, but the server's heartbeat arrives every 15 s.
    for (const at of [15_000, 30_000, 45_000, 60_000]) {
      jest.setSystemTime(start + at);
      socket.receive({ type: "heartbeat" });
    }
    jest.setSystemTime(start + 70_000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(socket.closedWith).toBeUndefined();
    expect(sockets.sockets).toHaveLength(1);
    // A heartbeat writes nothing to the cache and moves no change time.
    expect(queryClient.getQueryData<DeliveryTimelineResponse>(key)).toEqual(body);
    expect(live.state().changedAt).toBe("2026-10-01T00:00:00Z");

    // Then the heartbeats stop: 46 s after the last one, the tab coming back replaces the socket.
    jest.setSystemTime(start + 106_000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(socket.closedWith).toBe(4000);
    jest.advanceTimersByTime(0);
    expect(sockets.sockets).toHaveLength(2);
  } finally {
    unsubscribe();
  }
});

test("state is open with the last change time, reconnecting since <t>, or closed", async () => {
  jest.useFakeTimers();
  const start = Date.parse("2026-10-12T10:00:00Z");
  jest.setSystemTime(start);
  expect(live.state()).toEqual({
    state: "closed",
    since: null,
    stuck: false,
    changedAt: null,
    verdict: null,
  });

  const { unsubscribe } = observe();
  const { socket, sub } = await opened();
  socket.receive(snapshot(sub, "40", "2026-10-12T09:58:00Z"));
  expect(live.state()).toEqual({
    state: "open",
    since: null,
    stuck: false,
    changedAt: "2026-10-12T09:58:00Z",
    verdict: null,
  });
  socket.receive(merged(sub));
  expect(live.state().changedAt).toBe("2026-10-01T00:01:00Z");

  const verdict = {
    type: "freshness" as const,
    freshness: { ...body.freshness, last_error: "rate limited" },
    stale: ["prs"],
  };
  socket.receive(verdict);
  expect(live.state().verdict).toEqual({ freshness: verdict.freshness, stale: ["prs"] });

  jest.setSystemTime(start + 5_000);
  socket.drop(1006);
  expect(live.state()).toMatchObject({ state: "reconnecting", since: start + 5_000 });

  unsubscribe();
  await settle();
  expect(live.state().state).toBe("closed");
});

test("for each run a delta upserts, an open drill-down's run is read again", async () => {
  const { unsubscribe } = observe();
  let runReads = 0;
  const run = new QueryObserver(queryClient, {
    queryKey: ["delivery", "run", 42],
    queryFn: async () => {
      runReads += 1;
      return { id: 42, url: "", jobs: [] };
    },
  });
  const stopRun = run.subscribe(() => {});
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));
    await settle();
    expect(runReads).toBe(1);

    // A delta that changes no run reads nothing; one that upserts run 42 reads its drill-down.
    socket.receive(merged(sub));
    await settle();
    expect(runReads).toBe(1);
    socket.receive({
      ...merged(sub, "42"),
      items: { runs: { upsert: [{ id: 42 } as never], remove: [] } },
      counts: {},
      revs: { items: { runs: { "42": "cccccccccccccccc" } } },
    });
    await settle();
    expect(runReads).toBe(2);
  } finally {
    stopRun();
    unsubscribe();
  }
});

test("refreshQueries neither cancels nor invalidates a live query on its first load", async () => {
  let ordinaryCalls = 0;
  const ordinary = new QueryObserver(queryClient, {
    queryKey: ["issues"],
    queryFn: () => {
      ordinaryCalls += 1;
      return new Promise<string>(() => {});
    },
  });
  const stopOrdinary = ordinary.subscribe(() => {});
  const subscribed: unknown[] = [];
  const subscribe = live.subscribe.bind(live);
  live.subscribe = ((queryKey, liveView) => {
    subscribed.push(queryKey);
    return subscribe(queryKey, liveView);
  }) as LiveSocket["subscribe"];
  const { unsubscribe } = observe();
  try {
    await settle();
    expect(subscribed).toHaveLength(1);
    expect(ordinaryCalls).toBe(1);

    refreshQueries(queryClient);
    await settle();
    // The live query's first load is still the one in flight, its queryFn run once.
    expect(queryClient.getQueryState(key)?.fetchStatus).toBe("fetching");
    expect(queryClient.getQueryState(key)?.isInvalidated).toBe(false);
    expect(subscribed).toHaveLength(1);
    // The ordinary one is cancelled and fetched again, as before.
    expect(ordinaryCalls).toBe(2);

    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));
    await settle();
    expect(queryClient.getQueryData<DeliveryTimelineResponse>(key)).toEqual(body);
    refreshQueries(queryClient);
    await settle();
    expect(queryClient.getQueryState(key)?.isInvalidated).toBe(false);
    expect(subscribed).toHaveLength(1);
  } finally {
    stopOrdinary();
    unsubscribe();
  }
});

test("reconnectDelayMs doubles from a 1s base and caps at 30s", () => {
  expect(reconnectDelayMs(0)).toBe(1_000);
  expect(reconnectDelayMs(1)).toBe(2_000);
  expect(reconnectDelayMs(2)).toBe(4_000);
  expect(reconnectDelayMs(3)).toBe(8_000);
  expect(reconnectDelayMs(10)).toBe(30_000);
});

/** An event feed as the page keeps one: it adopts `events_start`'s id while it holds no cursor and
 *  each event's id after that, and records every call the socket makes - each start's `afterGap`
 *  in `gaps`. */
function eventFeed() {
  let cursor: string | null = null;
  const starts: string[] = [];
  const gaps: boolean[] = [];
  const events: { id: string; event: unknown }[] = [];
  const handlers: EventsHandlers = {
    since: () => cursor,
    onStart: (since, afterGap) => {
      starts.push(since);
      gaps.push(afterGap);
      cursor ??= since;
    },
    onEvent: (id, event) => {
      events.push({ id, event });
      cursor = id;
    },
  };
  return { handlers, starts, gaps, events };
}

/** Dispatch starts the event feed `socket` carries after `since`. */
function startFeed(socket: FakeLiveSocket, since = "0") {
  socket.receive({ type: "events_start", sub: socket.subscribed("events").sub, since });
}

/** An agent live view's handlers, recording every call in order. */
function agentView() {
  const seen: [string, unknown][] = [];
  const handlers: AgentHandlers = {
    onResponder: (data) => {
      seen.push(["responder", data]);
    },
    onReplay: (data) => {
      seen.push(["replay", data]);
    },
    onFrame: (data) => {
      seen.push(["frame", data]);
    },
    onUnavailable: (code, message) => {
      seen.push(["unavailable", { code, message }]);
    },
  };
  return { handlers, seen };
}

/** The subscribe frame `socket` sent last for session `id`'s agent view. */
function agentSubscription(socket: FakeLiveSocket, id: string) {
  const frame = socket
    .subscribes()
    .filter((sent) => sent.view.kind === "agent" && sent.view.session_id === id)
    .at(-1);
  if (frame === undefined) throw new Error(`no agent subscription for ${id} was sent`);
  return frame;
}

/** Fails every socket the page opens from here on, as a server that refuses every handshake does,
 *  while the clock runs `ms` forward. */
function refuseEveryOpen(ms: number, step = 500) {
  for (let ran = 0; ran < ms; ran += step) {
    jest.advanceTimersByTime(step);
    const socket = sockets.sockets.at(-1);
    if (socket !== undefined && socket.readyState === 0) socket.drop(1006);
  }
}

test("the first open shows no reconnecting state", async () => {
  const states: string[] = [];
  const stopStates = live.onState((state) => {
    states.push(state.state);
  });
  const { unsubscribe } = observe();
  try {
    await settle();
    expect(live.state()).toMatchObject({ state: "connecting", since: null, stuck: false });
    sockets.latest().open();
    expect(live.state()).toMatchObject({ state: "open", since: null, stuck: false });
    expect(states).toContain("connecting");
    expect(states).not.toContain("reconnecting");
  } finally {
    stopStates();
    unsubscribe();
  }
});

test("a 4001 reopens at once, keeps state open and resubscribes every kind", async () => {
  const { unsubscribe } = observe();
  const feed = eventFeed();
  const agent = agentView();
  const stopFeed = live.subscribeEvents(feed.handlers);
  const stopAgent = live.subscribeAgent(SESSION_A, agent.handlers);
  const states: string[] = [];
  const stopStates = live.onState((state) => {
    states.push(state.state);
  });
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));
    const events = socket.subscribed("events").sub;
    const agentSub = agentSubscription(socket, SESSION_A).sub;
    socket.receive({ type: "events_start", sub: events, since: "7" });

    socket.drop(4001);
    // No backoff, and nothing for a pill or the freshness row to show.
    expect(sockets.sockets).toHaveLength(2);
    expect(live.state()).toMatchObject({ state: "open", since: null, stuck: false });
    const again = sockets.latest();
    again.open();
    expect(again.subscribed("timeline")).toEqual({
      type: "subscribe",
      sub,
      view,
      resume: { seq: "40", revs },
    });
    expect(again.subscribed("events")).toEqual({
      type: "subscribe",
      sub: events,
      view: { kind: "events" },
      resume: { since: "7" },
    });
    expect(again.subscribed("agent")).toEqual({
      type: "subscribe",
      sub: agentSub,
      view: { kind: "agent", session_id: SESSION_A },
    });
    expect(again.subscribes()).toHaveLength(3);
    startFeed(again, "7");
    expect(feed.gaps).toEqual([false, false]);
    expect(states.filter((state) => state !== "open")).toEqual([]);
  } finally {
    stopStates();
    stopAgent();
    stopFeed();
    unsubscribe();
  }
});

test("a 1012 reopens at once and reports a gap to the event feed", () => {
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  try {
    sockets.latest().open();
    startFeed(sockets.latest());
    expect(feed.gaps).toEqual([false]);

    sockets.latest().drop(1012);
    expect(live.state().state).toBe("reconnecting");
    expect(sockets.sockets).toHaveLength(2);
    sockets.latest().open();
    expect(live.state()).toMatchObject({ state: "open", since: null });
    startFeed(sockets.latest());
    expect(feed.gaps).toEqual([false, true]);
  } finally {
    stop();
  }
});

test("a resubscribe after a rotation sends the since the event feed adopted from events_start", () => {
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  try {
    const socket = sockets.latest();
    socket.open();
    // A page that has applied no event sends no resume.
    const { sub } = socket.subscribed("events");
    expect(socket.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
    });
    socket.receive({ type: "events_start", sub, since: "120" });
    expect(feed.starts).toEqual(["120"]);

    socket.drop(4001);
    const again = sockets.latest();
    again.open();
    expect(again.subscribed("events")).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
      resume: { since: "120" },
    });
  } finally {
    stop();
  }
});

test("an ended frame resubscribes only its subscription", async () => {
  const { unsubscribe } = observe();
  const feed = eventFeed();
  const agent = agentView();
  const stopFeed = live.subscribeEvents(feed.handlers);
  const stopAgent = live.subscribeAgent(SESSION_A, agent.handlers);
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));
    const events = socket.subscribed("events").sub;
    const agentSub = agentSubscription(socket, SESSION_A).sub;
    socket.receive({ type: "events_start", sub: events, since: "9" });
    const before = socket.subscribes().length;

    socket.receive({ type: "ended", sub: events, reason: "behind" });
    expect(socket.subscribes().slice(before)).toEqual([
      { type: "subscribe", sub: events, view: { kind: "events" }, resume: { since: "9" } },
    ]);
    // Dispatch carried nothing for it in between, so its next start reports a gap.
    startFeed(socket, "9");
    expect(feed.gaps).toEqual([false, true]);

    socket.receive({ type: "ended", sub: agentSub, reason: "dropped" });
    expect(socket.subscribes().slice(before + 1)).toEqual([
      { type: "subscribe", sub: agentSub, view: { kind: "agent", session_id: SESSION_A } },
    ]);
    // The socket itself stays: nothing reconnected, and no other subscription was sent again.
    expect(sockets.sockets).toHaveLength(1);
    expect(socket.closedWith).toBeUndefined();
    expect(live.state().state).toBe("open");
  } finally {
    stopAgent();
    stopFeed();
    unsubscribe();
  }
});

test("a failed handshake invalidates whoami at most every 30 s", () => {
  jest.useFakeTimers();
  // No jitter: each retry waits exactly reconnectDelayMs, so the attempts fail at 0, 1, 3, 7, 15,
  // 31 and 61 s.
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const whoami = ["whoami"];
  const signedIn = () => queryClient.setQueryData(whoami, { kind: "user", login: "alice" });
  const invalidated = () => queryClient.getQueryState(whoami)?.isInvalidated === true;
  signedIn();
  const stop = live.subscribeEvents(eventFeed().handlers);
  try {
    const start = Date.now();
    const invalidations: number[] = [];
    sockets.latest().fail();
    for (let ran = 0; ran < 62_000; ran += 500) {
      if (invalidated()) {
        invalidations.push(Date.now() - start);
        signedIn();
      }
      refuseEveryOpen(500);
    }
    expect(sockets.sockets).toHaveLength(7);
    expect(invalidations).toEqual([0, 31_000, 61_000]);
  } finally {
    random.mockRestore();
    stop();
  }
});

test("a 1008 invalidates whoami and reconnects with backoff", () => {
  jest.useFakeTimers();
  const whoami = ["whoami"];
  queryClient.setQueryData(whoami, { kind: "user", login: "alice" });
  const stop = live.subscribeEvents(eventFeed().handlers);
  try {
    sockets.latest().open();
    expect(queryClient.getQueryState(whoami)?.isInvalidated).toBe(false);
    sockets.latest().drop(1008);
    expect(queryClient.getQueryState(whoami)?.isInvalidated).toBe(true);
    expect(live.state().state).toBe("reconnecting");
    expect(sockets.sockets).toHaveLength(1);
    jest.advanceTimersByTime(1_250);
    expect(sockets.sockets).toHaveLength(2);
  } finally {
    stop();
  }
});

test("a socket silent for 45 s while open is replaced", () => {
  jest.useFakeTimers();
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  try {
    const socket = sockets.latest();
    socket.open();
    startFeed(socket);
    // The check runs every 15 s, the tab never hidden: at 45 s the socket has been silent exactly
    // as long as it may be.
    jest.advanceTimersByTime(45_000);
    expect(socket.closedWith).toBeUndefined();
    expect(sockets.sockets).toHaveLength(1);
    jest.advanceTimersByTime(15_000);
    expect(socket.closedWith).toBe(4000);
    expect(sockets.sockets).toHaveLength(2);
    sockets.latest().open();
    startFeed(sockets.latest());
    expect(feed.gaps).toEqual([false, true]);

    // The server's heartbeat, every 15 s, keeps the new one.
    for (let beat = 0; beat < 8; beat += 1) {
      jest.advanceTimersByTime(15_000);
      sockets.latest().receive({ type: "heartbeat" });
    }
    expect(sockets.sockets).toHaveLength(2);
    expect(sockets.latest().closedWith).toBeUndefined();
  } finally {
    stop();
  }
});

test("two minutes of failed reconnects read as stuck, and the next open clears it", () => {
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const stop = live.subscribeEvents(eventFeed().handlers);
  try {
    sockets.latest().open();
    const lostAt = Date.now();
    sockets.latest().drop(1006);
    expect(live.state()).toMatchObject({ state: "reconnecting", since: lostAt, stuck: false });

    refuseEveryOpen(119_500);
    expect(live.state()).toMatchObject({ state: "reconnecting", since: lostAt, stuck: false });
    refuseEveryOpen(500);
    expect(live.state()).toMatchObject({ state: "reconnecting", since: lostAt, stuck: true });

    // The page keeps trying underneath: the next attempt, at 121 s, opens.
    const attempts = sockets.sockets.length;
    jest.advanceTimersByTime(1_000);
    expect(sockets.sockets).toHaveLength(attempts + 1);
    sockets.latest().open();
    expect(live.state()).toMatchObject({ state: "open", since: null, stuck: false });
  } finally {
    random.mockRestore();
    stop();
  }
});

test("one socket carries a delivery view, the event feed and two agent views", async () => {
  const { observer, unsubscribe } = observe();
  const feed = eventFeed();
  const first = agentView();
  const second = agentView();
  const stopFeed = live.subscribeEvents(feed.handlers);
  const stopFirst = live.subscribeAgent(SESSION_A, first.handlers);
  const stopSecond = live.subscribeAgent(SESSION_B, second.handlers);
  try {
    const { socket, sub } = await opened();
    expect(sockets.sockets).toHaveLength(1);
    const frames = socket.subscribes();
    expect(frames.map((frame) => frame.view)).toEqual(
      expect.arrayContaining([
        view,
        { kind: "events" },
        { kind: "agent", session_id: SESSION_A },
        { kind: "agent", session_id: SESSION_B },
      ])
    );
    expect(new Set(frames.map((frame) => frame.sub)).size).toBe(4);

    const events = socket.subscribed("events").sub;
    const agentA = agentSubscription(socket, SESSION_A).sub;
    const agentB = agentSubscription(socket, SESSION_B).sub;
    const event = { id: 11, type: "issue.updated" } as unknown as LiveEventFrame["event"];
    const frame = { commands: [], kind: "commands", seq: 1, v: 1 } satisfies AgentStreamFrame;
    socket.receive(snapshot(sub));
    socket.receive({ type: "events_start", sub: events, since: "10" });
    socket.receive({ type: "event", sub: events, id: "11", event });
    socket.receive({
      type: "agent",
      sub: agentA,
      event: "responder",
      data: { v: 1, responding: true },
    });
    socket.receive({
      type: "agent",
      sub: agentB,
      event: "replay",
      data: { v: 1, session_id: SESSION_B, frames: [] },
    });
    socket.receive({ type: "agent", sub: agentA, event: "frame", data: frame });
    await settle();

    expect(observer.getCurrentResult().data).toEqual(body);
    expect(feed.starts).toEqual(["10"]);
    expect(feed.events).toEqual([{ id: "11", event }]);
    expect(first.seen).toEqual([
      ["responder", { v: 1, responding: true }],
      ["frame", frame],
    ]);
    expect(second.seen).toEqual([["replay", { v: 1, session_id: SESSION_B, frames: [] }]]);

    // Leaving one agent view unsubscribes it alone, and the socket stays for the rest.
    stopFirst();
    expect(socket.unsubscribes()).toEqual([{ type: "unsubscribe", sub: agentA }]);
    await settle();
    expect(socket.closedWith).toBeUndefined();
  } finally {
    stopSecond();
    stopFirst();
    stopFeed();
    unsubscribe();
  }
});

test("an agent error frame reaches onUnavailable and is not retried", () => {
  const agent = agentView();
  const stopFeed = live.subscribeEvents(eventFeed().handlers);
  const stopAgent = live.subscribeAgent(SESSION_A, agent.handlers);
  try {
    const socket = sockets.latest();
    socket.open();
    const { sub } = agentSubscription(socket, SESSION_A);
    socket.receive({
      type: "error",
      sub,
      code: "STREAM_UNAVAILABLE",
      message: "the session's stream is not reachable",
    });
    expect(agent.seen).toEqual([
      [
        "unavailable",
        { code: "STREAM_UNAVAILABLE", message: "the session's stream is not reachable" },
      ],
    ]);
    expect(socket.subscribes()).toHaveLength(2);

    // A reconnect resubscribes the feed and not the refused view, which the page no longer holds.
    socket.drop(1012);
    sockets.latest().open();
    expect(
      sockets
        .latest()
        .subscribes()
        .map((frame) => frame.view.kind)
    ).toEqual(["events"]);
    stopAgent();
    expect(sockets.latest().unsubscribes()).toEqual([]);
    expect(agent.seen).toHaveLength(1);
  } finally {
    stopAgent();
    stopFeed();
  }
});

test("a handshake that never finishes is replaced after 45 s, and the event feed hears of the gap", () => {
  jest.useFakeTimers();
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  try {
    const first = sockets.latest();
    jest.advanceTimersByTime(45_000);
    expect(first.closedWith).toBeUndefined();
    jest.advanceTimersByTime(15_000);
    expect(first.closedWith).toBe(4000);
    expect(live.state().state).toBe("reconnecting");
    expect(sockets.sockets).toHaveLength(2);
    // The feed's first start follows an attempt that carried nothing, so it reports a gap.
    sockets.latest().open();
    startFeed(sockets.latest());
    expect(feed.gaps).toEqual([true]);
  } finally {
    stop();
  }
});

test("going offline closes the socket and shows reconnecting", () => {
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  try {
    const socket = sockets.latest();
    socket.open();
    const lostAt = Date.now();
    window.dispatchEvent(new Event("offline"));
    expect(socket.closedWith).toBe(4000);
    expect(live.state()).toMatchObject({ state: "reconnecting", since: lostAt, stuck: false });
    // A frame still in flight on the socket the page let go writes nothing.
    socket.receive({ type: "events_start", sub: socket.subscribed("events").sub, since: "3" });
    expect(feed.starts).toEqual([]);
    // Nothing reopens at once: the page waits out its backoff, or for the network to come back.
    expect(sockets.sockets).toHaveLength(1);
    jest.advanceTimersByTime(999);
    expect(sockets.sockets).toHaveLength(1);
    jest.advanceTimersByTime(1);
    expect(sockets.sockets).toHaveLength(2);
    sockets.latest().open();
    startFeed(sockets.latest());
    expect(feed.gaps).toEqual([true]);

    // A handshake still out when the network goes is let go the same way.
    sockets.latest().drop(1006);
    jest.advanceTimersByTime(2_000);
    expect(sockets.sockets).toHaveLength(3);
    window.dispatchEvent(new Event("offline"));
    expect(sockets.latest().closedWith).toBe(4000);
    expect(live.state().state).toBe("reconnecting");
    // With no socket left, a second `offline` changes nothing.
    window.dispatchEvent(new Event("offline"));
    expect(sockets.sockets).toHaveLength(3);
  } finally {
    random.mockRestore();
    stop();
  }
});

test("coming online reopens at once", () => {
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  try {
    sockets.latest().open();
    // Three losses in a row: the next attempt waits four seconds.
    sockets.latest().drop(1006);
    jest.advanceTimersByTime(1_000);
    sockets.latest().drop(1006);
    jest.advanceTimersByTime(2_000);
    sockets.latest().drop(1006);
    const attempts = sockets.sockets.length;
    expect(attempts).toBe(3);

    window.dispatchEvent(new Event("online"));
    expect(sockets.sockets).toHaveLength(attempts + 1);
    // The backoff timer it preempted opens no second socket.
    jest.advanceTimersByTime(5_000);
    expect(sockets.sockets).toHaveLength(attempts + 1);
    // And the backoff starts over: this attempt failing waits one second again, not eight.
    sockets.latest().drop(1006);
    jest.advanceTimersByTime(1_000);
    expect(sockets.sockets).toHaveLength(attempts + 2);

    // A handshake still out, begun while the network was gone, is replaced at once.
    const pending = sockets.latest();
    window.dispatchEvent(new Event("online"));
    expect(pending.closedWith).toBe(4000);
    expect(sockets.sockets).toHaveLength(attempts + 3);
    sockets.latest().open();
    expect(live.state()).toMatchObject({ state: "open", since: null, stuck: false });
    startFeed(sockets.latest());
    expect(feed.gaps).toEqual([true]);

    // An open socket carrying frames is left alone.
    window.dispatchEvent(new Event("online"));
    expect(sockets.sockets).toHaveLength(attempts + 3);
    expect(sockets.latest().closedWith).toBeUndefined();
  } finally {
    random.mockRestore();
    stop();
  }
});

const endings = [
  ["the last subscription leaves during the backoff", "leave", "backoff"],
  ["the last subscription leaves during a handshake", "leave", "handshake"],
  ["close() is called during the backoff", "close", "backoff"],
  ["close() is called during a handshake", "close", "handshake"],
] as const;

test.each(
  endings
)("once %s, no timer or window event opens a socket or reports a state", async (_case, end, moment) => {
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const stop = live.subscribeEvents(eventFeed().handlers);
  try {
    sockets.latest().open();
    // The stuck clock runs from this loss, and the first retry is a second away.
    sockets.latest().drop(1006);
    if (moment === "handshake") {
      // The retry's handshake is out, with its silence check armed.
      jest.advanceTimersByTime(1_000);
    }
    expect(sockets.sockets).toHaveLength(moment === "handshake" ? 2 : 1);
    expect(live.state().state).toBe("reconnecting");

    if (end === "leave") stop();
    else live.close();
    await settle();
    expect(live.state()).toMatchObject({ state: "closed", since: null, stuck: false });

    const opened = sockets.sockets.length;
    const reported: LiveSocketState[] = [];
    const stopStates = live.onState((state) => {
      reported.push(state);
    });
    // The browser's late close event for the socket the page let go.
    sockets.latest().drop(1006);
    jest.advanceTimersByTime(2_000);
    jest.advanceTimersByTime(LIVE_SOCKET_STUCK_MS);
    jest.advanceTimersByTime(60_000);
    document.dispatchEvent(new Event("visibilitychange"));
    window.dispatchEvent(new Event("online"));
    window.dispatchEvent(new Event("offline"));
    jest.advanceTimersByTime(60_000);
    expect(sockets.sockets).toHaveLength(opened);
    expect(reported).toEqual([]);
    stopStates();
  } finally {
    random.mockRestore();
    stop();
  }
});

test("an error frame for the event feed resubscribes it after the socket's backoff", () => {
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  const refusal = (sub: string) =>
    ({ type: "error", sub, code: "STREAM_UNAVAILABLE", message: "no feed" }) as const;
  try {
    const socket = sockets.latest();
    socket.open();
    const { sub } = socket.subscribed("events");
    socket.receive({ type: "events_start", sub, since: "30" });
    socket.receive(refusal(sub));
    expect(socket.subscribes()).toHaveLength(1);
    jest.advanceTimersByTime(999);
    expect(socket.subscribes()).toHaveLength(1);
    jest.advanceTimersByTime(1);
    expect(socket.subscribes()).toHaveLength(2);
    expect(socket.subscribes().at(-1)).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
      resume: { since: "30" },
    });
    // No send reports the gap.
    expect(feed.gaps).toEqual([false]);

    // A second refusal in a row waits twice as long.
    socket.receive(refusal(sub));
    jest.advanceTimersByTime(1_999);
    expect(socket.subscribes()).toHaveLength(2);
    jest.advanceTimersByTime(1);
    expect(socket.subscribes()).toHaveLength(3);

    // A subscription that starts again reports the gap, once, and resets the wait.
    socket.receive({ type: "events_start", sub, since: "30" });
    expect(feed.gaps).toEqual([false, true]);
    socket.receive(refusal(sub));
    jest.advanceTimersByTime(1_000);
    expect(socket.subscribes()).toHaveLength(4);
    // The socket itself stayed throughout.
    expect(sockets.sockets).toHaveLength(1);
    expect(socket.closedWith).toBeUndefined();
    expect(live.state().state).toBe("open");

    // A retry still waiting when the socket is lost is sent once, by the next socket's open.
    socket.receive(refusal(sub));
    socket.drop(1012);
    sockets.latest().open();
    expect(sockets.latest().subscribes()).toHaveLength(1);
    jest.advanceTimersByTime(10_000);
    expect(sockets.latest().subscribes()).toHaveLength(1);

    // Leaving while a retry waits tells Dispatch nothing: it holds no subscription it refused.
    sockets.latest().receive(refusal(sub));
    stop();
    expect(sockets.latest().unsubscribes()).toEqual([]);
    jest.advanceTimersByTime(10_000);
    expect(sockets.latest().subscribes()).toHaveLength(1);
  } finally {
    random.mockRestore();
    stop();
  }
});

// While Dispatch cannot start the feed - its database does not answer - it ends each subscription
// a second after it is sent. An end that follows a start is sent again at once; ends with no start
// since the last send wait out the same doubling backoff a refusal does. The gap is reported once,
// when the feed starts again, not at every send.
test("ended frames with no events_start back off 1 s, 2 s, 4 s and the next start reports the gap once", () => {
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const feed = eventFeed();
  const stop = live.subscribeEvents(feed.handlers);
  const ended = (sub: string) => ({ type: "ended", sub, reason: "behind" }) as const;
  try {
    const socket = sockets.latest();
    socket.open();
    const { sub } = socket.subscribed("events");
    socket.receive({ type: "events_start", sub, since: "30" });
    expect(feed.gaps).toEqual([false]);

    // The feed had started, so its end is sent again at once.
    socket.receive(ended(sub));
    expect(socket.subscribes()).toHaveLength(2);

    for (const [index, wait] of [1_000, 2_000, 4_000].entries()) {
      socket.receive(ended(sub));
      jest.advanceTimersByTime(wait - 1);
      expect(socket.subscribes()).toHaveLength(2 + index);
      jest.advanceTimersByTime(1);
      expect(socket.subscribes()).toHaveLength(3 + index);
    }
    expect(socket.subscribes().at(-1)).toEqual({
      type: "subscribe",
      sub,
      view: { kind: "events" },
      resume: { since: "30" },
    });
    // No send reported the gap.
    expect(feed.gaps).toEqual([false]);

    // The start reports it, once, and resets the wait: the next end is sent again at once.
    socket.receive({ type: "events_start", sub, since: "30" });
    expect(feed.gaps).toEqual([false, true]);
    socket.receive(ended(sub));
    expect(socket.subscribes()).toHaveLength(6);
    expect(sockets.sockets).toHaveLength(1);
  } finally {
    random.mockRestore();
    stop();
  }
});

test("the connection pill holds its value through a delta with a new change time", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const shared = liveSocketFor(client);
  const observer = new QueryObserver(client, {
    queryKey: key,
    queryFn: ({ queryKey: k }) => shared.subscribe(k, view),
    meta: { live: true },
    staleTime: Number.POSITIVE_INFINITY,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    retry: false,
  });
  const stopObserver = observer.subscribe(() => {});
  let renders = 0;
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  const pill = renderHook(
    () => {
      renders += 1;
      return useLiveSocketPill();
    },
    { wrapper }
  );
  try {
    await settle();
    const socket = sockets.latest();
    act(() => socket.open());
    const { sub } = socket.subscribed("timeline");
    act(() => socket.receive(snapshot(sub)));
    expect(pill.result.current).toBe("none");
    const rendered = renders;

    act(() => socket.receive(merged(sub)));
    // The delta moved the change time the freshness row reads, and nothing the pill shows.
    expect(shared.state().changedAt).toBe("2026-10-01T00:01:00Z");
    expect(renders).toBe(rendered);
    expect(pill.result.current).toBe("none");

    act(() => socket.drop(1006));
    expect(pill.result.current).toBe("reconnecting");
  } finally {
    pill.unmount();
    stopObserver();
    shared.close();
    client.clear();
  }
});
