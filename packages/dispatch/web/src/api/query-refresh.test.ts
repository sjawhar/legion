import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { QueryClient, type QueryKey, QueryObserver } from "@tanstack/react-query";

import { MIN_REFETCH_INTERVAL_MS, refreshQueries, trackQueryFetches } from "./query-refresh";

// The scheduler runs on the clock: every test drives fake timers, and each answer from the server
// is a timer too, so a fetch takes exactly as long as the test says.
beforeEach(() => {
  jest.useFakeTimers();
});
afterEach(() => {
  jest.useRealTimers();
});

/** Lets every promise chain that a timer released run to its end. */
async function settle(): Promise<void> {
  for (let index = 0; index < 20; index += 1) {
    await Promise.resolve();
  }
}

/** Moves the clock in 10 ms steps, settling the promises each step releases. */
async function advance(ms: number): Promise<void> {
  for (let elapsed = 0; elapsed < ms; elapsed += 10) {
    jest.advanceTimersByTime(Math.min(10, ms - elapsed));
    await settle();
  }
}

interface ServedQuery {
  /** When each fetch began, in ms since the test's start. */
  readonly starts: number[];
  /** The most requests for this query ever in flight at once. */
  readonly mostInFlight: () => number;
  readonly stop: () => void;
}

/**
 * A rendered query whose every fetch answers `respond(n)` after `latencyMs`. `n` counts fetches
 * from 1.
 */
function servedQuery(
  queryClient: QueryClient,
  queryKey: QueryKey,
  latencyMs: number,
  respond: (fetch: number) => unknown = (fetch) => fetch,
  meta?: Record<string, unknown>
): ServedQuery {
  const origin = Date.now();
  const starts: number[] = [];
  let inFlight = 0;
  let most = 0;
  const observer = new QueryObserver(queryClient, {
    meta,
    queryFn: () => {
      starts.push(Date.now() - origin);
      inFlight += 1;
      most = Math.max(most, inFlight);
      const fetch = starts.length;
      const { promise, resolve } = Promise.withResolvers<unknown>();
      setTimeout(() => {
        inFlight -= 1;
        resolve(respond(fetch));
      }, latencyMs);
      return promise;
    },
    queryKey,
    retry: false,
    staleTime: 30_000,
  });
  const unsubscribe = observer.subscribe(() => {});
  return { mostInFlight: () => most, starts, stop: unsubscribe };
}

function newClient(): QueryClient {
  const queryClient = new QueryClient();
  trackQueryFetches(queryClient);
  return queryClient;
}

test("events every 100 ms fetch a read at most once a second, the last within a second of the last event", async () => {
  const queryClient = newClient();
  const origin = Date.now();
  const list = servedQuery(queryClient, ["issues", "project", "CORE"], 50);
  try {
    await advance(200);
    let lastEvent = 0;
    for (let index = 0; index < 30; index += 1) {
      refreshQueries(queryClient, [{ queryKey: ["issues", "project", "CORE"] }]);
      lastEvent = Date.now() - origin;
      await advance(100);
    }
    await advance(3_000);

    for (let index = 1; index < list.starts.length; index += 1) {
      expect(list.starts[index] - list.starts[index - 1]).toBeGreaterThanOrEqual(
        MIN_REFETCH_INTERVAL_MS
      );
    }
    const last = list.starts[list.starts.length - 1];
    expect(last).toBeGreaterThan(lastEvent);
    expect(last - lastEvent).toBeLessThanOrEqual(MIN_REFETCH_INTERVAL_MS);
    // Three seconds of events cost a few fetches, not thirty.
    expect(list.starts.length).toBeLessThanOrEqual(5);
    expect(list.mostInFlight()).toBe(1);
  } finally {
    list.stop();
  }
});

test("events during a fetch give exactly one more fetch, once it settles", async () => {
  const queryClient = newClient();
  const detail = servedQuery(queryClient, ["issue", "CORE-1"], 400);
  try {
    // The first fetch is out from 0 to 400 ms; an event arrives at 100, 200 and 300 ms.
    for (let index = 0; index < 3; index += 1) {
      await advance(100);
      refreshQueries(queryClient, [{ exact: true, queryKey: ["issue", "CORE-1"] }]);
    }
    await advance(3_000);

    expect(detail.starts).toEqual([0, MIN_REFETCH_INTERVAL_MS]);
    expect(detail.mostInFlight()).toBe(1);
  } finally {
    detail.stop();
  }
});

test("a fetch slower than a second is fetched again the moment it settles", async () => {
  const queryClient = newClient();
  const inbox = servedQuery(queryClient, ["inbox"], 1_500);
  try {
    await advance(100);
    refreshQueries(queryClient, [{ queryKey: ["inbox"] }]);
    await advance(4_000);

    // It settles at 1,500 ms; the next fetch starts on the following tick, not at the 1 s mark.
    expect(inbox.starts).toHaveLength(2);
    expect(inbox.starts[1]).toBeGreaterThanOrEqual(1_500);
    expect(inbox.starts[1]).toBeLessThan(1_510);
    expect(inbox.mostInFlight()).toBe(1);
  } finally {
    inbox.stop();
  }
});

test("an idle read last fetched over a second ago is fetched at once", async () => {
  const queryClient = newClient();
  const detail = servedQuery(queryClient, ["issue", "CORE-1"], 50);
  try {
    await advance(2_000);
    refreshQueries(queryClient, [{ queryKey: ["issue", "CORE-1"] }]);
    await settle();

    expect(detail.starts).toEqual([0, 2_000]);
  } finally {
    detail.stop();
  }
});

test("a read nothing renders is only marked stale", async () => {
  const queryClient = newClient();
  let fetched = 0;
  // Once rendered and since unmounted, and never rendered at all.
  const unmounted = new QueryObserver(queryClient, {
    queryFn: async () => {
      fetched += 1;
      return "summary";
    },
    queryKey: ["issue-summary", "CORE-1"],
  });
  unmounted.subscribe(() => {})();
  queryClient.setQueryData(["issue-summary", "CORE-2"], "summary");
  await advance(2_000);
  fetched = 0;

  refreshQueries(queryClient, [{ queryKey: ["issue-summary"] }]);
  await advance(3_000);

  expect(fetched).toBe(0);
  expect(queryClient.getQueryState(["issue-summary", "CORE-1"])?.isInvalidated).toBe(true);
  expect(queryClient.getQueryState(["issue-summary", "CORE-2"])?.isInvalidated).toBe(true);
});

test("a live-owned read is never touched, by a target or by the whole-cache refresh", async () => {
  const queryClient = newClient();
  const live = servedQuery(queryClient, ["delivery", "timeline"], 50, undefined, { live: true });
  try {
    await advance(2_000);
    refreshQueries(queryClient, [{ queryKey: ["delivery"] }]);
    refreshQueries(queryClient);
    await advance(3_000);

    expect(live.starts).toEqual([0]);
    expect(queryClient.getQueryState(["delivery", "timeline"])?.isInvalidated).toBe(false);
  } finally {
    live.stop();
  }
});

test("the whole-cache refresh keeps the same rules: a recent fetch waits out its second, an idle read nothing renders is only marked stale", async () => {
  const queryClient = newClient();
  const projects = servedQuery(queryClient, ["projects"], 50);
  queryClient.setQueryData(["github", "acme/widgets/pull/1"], { state: "open" });
  try {
    await advance(300);
    refreshQueries(queryClient);
    await advance(3_000);

    expect(projects.starts).toEqual([0, MIN_REFETCH_INTERVAL_MS]);
    expect(queryClient.getQueryState(["github", "acme/widgets/pull/1"])?.isInvalidated).toBe(true);
  } finally {
    projects.stop();
  }
});

test("a test of a first load's data runs on the response that lands, and fetches again only when it holds", async () => {
  const queryClient = newClient();
  // Two Inbox reads on their first load: one lands holding a row of CORE-1's, one does not.
  const holds = servedQuery(queryClient, ["inbox"], 300, () => [{ issue_key: "CORE-1" }]);
  const lacks = servedQuery(queryClient, ["inbox", "OTHER"], 300, () => [{ issue_key: "OTHER-1" }]);
  try {
    await advance(100);
    refreshQueries(queryClient, [
      {
        queryKey: ["inbox"],
        when: (data) =>
          Array.isArray(data) &&
          data.some((row: { issue_key: string }) => row.issue_key === "CORE-1"),
      },
    ]);
    await advance(3_000);

    // Neither first load is cancelled; only the one whose body holds the issue is fetched again.
    expect(holds.starts).toEqual([0, MIN_REFETCH_INTERVAL_MS]);
    expect(lacks.starts).toEqual([0]);
  } finally {
    holds.stop();
    lacks.stop();
  }
});

test("a refresh owed to a removed read is not paid by the read that replaces it", async () => {
  const queryClient = newClient();
  const first = servedQuery(queryClient, ["issues", "pinned"], 500);
  await advance(100);
  refreshQueries(queryClient, [{ queryKey: ["issues", "pinned"] }]);
  first.stop();
  queryClient.removeQueries({ queryKey: ["issues", "pinned"] });
  await advance(100);

  const second = servedQuery(queryClient, ["issues", "pinned"], 100);
  try {
    await advance(3_000);
    expect(second.starts).toEqual([0]);
  } finally {
    second.stop();
  }
});

test("a read left inside the second after a fetch an event landed in still owes that event when it is rendered again", async () => {
  const queryClient = newClient();
  const key = ["issue-summary", "CORE-1"];
  const summary = servedQuery(queryClient, key, 300);
  await advance(1_200);
  // A first event fetches at 1,200 ms; a second lands at 1,300 ms while that fetch is out, whose
  // body was composed before it. The fetch settles at 1,500 ms and the owed one waits for 2,200.
  refreshQueries(queryClient, [{ exact: true, queryKey: key }]);
  await advance(100);
  refreshQueries(queryClient, [{ exact: true, queryKey: key }]);
  await advance(250);
  // The page is left inside that wait.
  summary.stop();
  await advance(2_000);

  expect(summary.starts).toEqual([0, 1_200]);
  // Nothing fetched the answer composed after the second event, so the read must still be stale,
  // or the page that renders it again shows the answer from before it.
  expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true);
});

test("a test of a read's data while a fetch is out runs on the response that lands, not the data it replaces", async () => {
  const queryClient = newClient();
  // The second fetch's body, composed before the test below, holds a row of CORE-1's; the data
  // on screen when the test runs does not.
  const inbox = servedQuery(queryClient, ["inbox"], 400, (fetch) =>
    fetch === 2 ? [{ issue_key: "OTHER-1" }, { issue_key: "CORE-1" }] : [{ issue_key: "OTHER-1" }]
  );
  try {
    await advance(1_500);
    refreshQueries(queryClient, [{ queryKey: ["inbox"] }]);
    await advance(100);
    refreshQueries(queryClient, [
      {
        queryKey: ["inbox"],
        when: (data) =>
          Array.isArray(data) &&
          data.some((row: { issue_key: string }) => row.issue_key === "CORE-1"),
      },
    ]);
    await advance(3_000);

    expect(inbox.starts).toEqual([0, 1_500, 2_500]);
    expect(inbox.mostInFlight()).toBe(1);
  } finally {
    inbox.stop();
  }
});
