import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { ApiError } from "./client";
import { eventRefreshTargets } from "./event-refresh";
import { MIN_REFETCH_INTERVAL_MS, refreshQueries, trackQueryFetches } from "./query-refresh";
import type { DispatchEvent } from "./types";

// The scheduler runs on the clock - it holds a second fetch of one query to
// `MIN_REFETCH_INTERVAL_MS` after the first began - so this drives fake timers.
beforeEach(() => {
  jest.useFakeTimers();
});
afterEach(() => {
  jest.useRealTimers();
});

/** Moves the clock in 10 ms steps, letting the promises each step releases run to their end. */
async function advance(ms: number): Promise<void> {
  for (let elapsed = 0; elapsed < ms; elapsed += 10) {
    jest.advanceTimersByTime(Math.min(10, ms - elapsed));
    for (let index = 0; index < 20; index += 1) {
      await Promise.resolve();
    }
  }
}

/** An issue write of `key` that names no `changed` list, so it moves the title and status. */
function issueEvent(type: "issue.created" | "issue.updated", key: string): DispatchEvent {
  const event = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-11-03T00:00:00Z",
    id: 1,
    issue_key: key,
    notify: true,
    payload: { key },
    project: key.slice(0, key.lastIndexOf("-")),
    seq: 1,
    type,
  };
  // The feed's event union is wider than this test needs; this is the shape its reader reads.
  return event as unknown as DispatchEvent;
}

test("a key whose summary answered 404 is not asked again on issue.updated, and is on issue.created", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  trackQueryFetches(queryClient);
  let fetches = 0;
  // The summary read of a key-shaped word a body writes bare that names no issue: it answers
  // 404, and something renders it, so a refresh that reaches it would fetch it again.
  const observer = new QueryObserver(queryClient, {
    queryFn: () => {
      fetches += 1;
      return Promise.reject(new ApiError(404, { code: "NOT_FOUND", error: "no issue" }));
    },
    queryKey: ["issue-summary", "CORE-404"],
    retry: false,
    staleTime: Number.POSITIVE_INFINITY,
  });
  const stop = observer.subscribe(() => {});

  try {
    await advance(10);
    expect(fetches).toBe(1);
    expect(observer.getCurrentResult().error).toBeInstanceOf(ApiError);

    // An update of an issue cannot make a key name one, so the 404 stands and nothing is asked.
    refreshQueries(queryClient, eventRefreshTargets(issueEvent("issue.updated", "CORE-404")));
    await advance(MIN_REFETCH_INTERVAL_MS + 100);
    expect(fetches).toBe(1);

    // A new issue can, so that event asks again whatever the last answer was.
    refreshQueries(queryClient, eventRefreshTargets(issueEvent("issue.created", "CORE-404")));
    await advance(MIN_REFETCH_INTERVAL_MS + 100);
    expect(fetches).toBe(2);
  } finally {
    stop();
  }
});
