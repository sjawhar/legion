import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import {
  type FakeDeliverySocket,
  type FakeDeliverySockets,
  installFakeDeliverySockets,
} from "../__tests__/delivery-live-harness";
import { ApiError, isDeliveryNotConfigured } from "./client";
import { createDeliveryLive, type DeliveryLive } from "./delivery-live";
import { refreshQueries } from "./query-refresh";
import type {
  DeliveryLiveBody,
  DeliveryLiveDelta,
  DeliveryLiveRevs,
  DeliveryLiveSnapshot,
  DeliveryLiveView,
  DeliveryPR,
  DeliveryTimelineResponse,
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

let sockets: FakeDeliverySockets;
let queryClient: QueryClient;
let live: DeliveryLive;

beforeEach(() => {
  sockets = installFakeDeliverySockets();
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  live = createDeliveryLive(queryClient);
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
async function opened(): Promise<{ socket: FakeDeliverySocket; sub: string }> {
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
    expect(socket.url).toBe(`${protocol}//${window.location.host}/ws/delivery`);
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

test("a socket silent past two heartbeats is replaced when the tab becomes visible", async () => {
  jest.useFakeTimers();
  const start = Date.now();
  jest.setSystemTime(start);
  const { unsubscribe } = observe();
  try {
    const { socket, sub } = await opened();
    socket.receive(snapshot(sub));

    jest.setSystemTime(start + 29_000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(sockets.sockets).toHaveLength(1);
    expect(socket.closedWith).toBeUndefined();

    jest.setSystemTime(start + 31_000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(socket.closedWith).toBe(4000);
    jest.advanceTimersByTime(0);
    expect(sockets.sockets).toHaveLength(2);
    sockets.latest().open();
    expect(sockets.latest().subscribed("timeline").resume?.seq).toBe("40");
  } finally {
    unsubscribe();
  }
});

test("heartbeats keep a quiet socket from being replaced, and 31 s with no frame replaces it", async () => {
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

    // Then the heartbeats stop: 31 s after the last one, the tab coming back replaces the socket.
    jest.setSystemTime(start + 91_000);
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
  expect(live.state()).toEqual({ state: "closed", since: null, changedAt: null, verdict: null });

  const { unsubscribe } = observe();
  const { socket, sub } = await opened();
  socket.receive(snapshot(sub, "40", "2026-10-12T09:58:00Z"));
  expect(live.state()).toEqual({
    state: "open",
    since: null,
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
  }) as DeliveryLive["subscribe"];
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
