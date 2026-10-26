import type { Query, QueryClient, QueryKey } from "@tanstack/react-query";

/** Whether a query is owned by Dispatch's live delivery socket rather than refreshed by events. */
export function isLiveQuery(query: Pick<Query, "meta">): boolean {
  return query.meta?.live === true;
}

/**
 * The one refresh scheduler: what happens to the queries an event batch names (`api/sse.ts` asks,
 * `api/event-refresh.ts` decides which). It never cancels a request, and it fetches one query at
 * most once a second:
 *
 * - an inactive query is marked stale only, and fetches when something mounts it;
 * - a query already fetching is fetched again once that fetch settles, so the answer composed
 *   after the event is the one shown, and the response already on its way is never discarded;
 * - a query whose last fetch began less than `MIN_REFETCH_INTERVAL_MS` ago is fetched at that
 *   start plus the interval;
 * - any other query is fetched now.
 *
 * When a fetch takes longer than the interval, the in-flight rule wins: the next fetch starts when
 * it settles. A refresh is owed from the moment it is asked for, and any fetch that begins later -
 * one this scheduler starts, a mount's, a mutation's - pays it. The live-owned queries
 * (`meta.live`, `api/delivery-live.ts`) are never touched: their socket keeps them current.
 */

/** The shortest gap between the starts of two fetches of one query that this scheduler starts. */
export const MIN_REFETCH_INTERVAL_MS = 1_000;

/**
 * Which queries a refresh reaches: every query `queryKey` matches (by prefix unless `exact`), and,
 * with `when`, only those whose data it accepts. `when` runs against the query's data at the
 * flush, and against `undefined` for a query that holds none and is not fetching (an error, a
 * query never fetched). For a query still on its first load it runs on the response that lands,
 * which is then fetched again if `when` accepts it; a body composed after the event is not.
 */
export interface RefreshTarget {
  readonly queryKey: QueryKey;
  readonly exact?: boolean;
  readonly when?: (data: unknown) => boolean;
}

interface DeferredCheck {
  readonly requestedAt: number;
  readonly when: readonly ((data: unknown) => boolean)[];
}

// Per QueryClient, keyed by query hash: when each query's last fetch began (from the cache's
// `fetch` action), the moment each owed refresh was asked for, the timer that will start a
// refresh the interval holds back, and the checks waiting for a first load to land. Every entry
// of a hash leaves when its query is removed.
interface SchedulerState {
  readonly started: Map<string, number>;
  readonly owed: Map<string, number>;
  readonly timers: Map<string, number>;
  readonly deferred: Map<string, DeferredCheck>;
}

const schedulers = new WeakMap<QueryClient, SchedulerState>();

function schedulerFor(queryClient: QueryClient): SchedulerState {
  const existing = schedulers.get(queryClient);
  if (existing !== undefined) {
    return existing;
  }
  const state: SchedulerState = {
    deferred: new Map(),
    owed: new Map(),
    started: new Map(),
    timers: new Map(),
  };
  schedulers.set(queryClient, state);
  const cache = queryClient.getQueryCache();
  // One subscription for the client, not one per query, so nothing can be stranded.
  cache.subscribe((change) => {
    const query = change.query;
    const hash = query.queryHash;
    if (change.type === "removed") {
      window.clearTimeout(state.timers.get(hash));
      state.timers.delete(hash);
      state.started.delete(hash);
      state.owed.delete(hash);
      state.deferred.delete(hash);
      return;
    }
    if (change.type !== "updated") {
      return;
    }
    const action = change.action;
    if (action.type === "fetch") {
      state.started.set(hash, Date.now());
      return;
    }
    if (action.type === "error") {
      // A first load that failed has no body to judge: the query shows its error and retries on
      // its own terms.
      state.deferred.delete(hash);
    }
    if (action.type === "success" && action.manual !== true) {
      const check = state.deferred.get(hash);
      if (check !== undefined) {
        state.deferred.delete(hash);
        const started = state.started.get(hash);
        const composedAfter = started !== undefined && started > check.requestedAt;
        if (!composedAfter && check.when.some((when) => when(query.state.data))) {
          owe(state, query, check.requestedAt);
        }
      }
    }
    // A query that owes a refresh settled: drive it, outside this notification, since the cache
    // is still dispatching the action that settled it.
    if (query.state.fetchStatus === "idle" && state.owed.has(hash) && !state.timers.has(hash)) {
      state.timers.set(
        hash,
        window.setTimeout(() => {
          state.timers.delete(hash);
          drive(state, queryClient, hash);
        }, 0)
      );
    }
  });
  return state;
}

/**
 * Starts recording when each query's fetches begin, so the first refresh after a page's own loads
 * already keeps the interval. `useEventStream` calls it once its stream starts; `refreshQueries`
 * starts it too, for any caller that has not.
 */
export function trackQueryFetches(queryClient: QueryClient): void {
  schedulerFor(queryClient);
}

/** Marks a query stale and, while something renders it, records that it owes a fetch begun after
 *  `requestedAt`. */
function owe(state: SchedulerState, query: Query, requestedAt: number): void {
  query.invalidate();
  if (!query.isActive() || query.isStatic()) {
    return;
  }
  const hash = query.queryHash;
  state.owed.set(hash, Math.max(state.owed.get(hash) ?? requestedAt, requestedAt));
}

/** Starts the fetch a query owes, now or once the in-flight and interval rules allow it. */
function drive(state: SchedulerState, queryClient: QueryClient, hash: string): void {
  const requestedAt = state.owed.get(hash);
  const query = queryClient.getQueryCache().get(hash);
  if (requestedAt === undefined || query === undefined) {
    return;
  }
  const started = state.started.get(hash);
  if (started !== undefined && started > requestedAt) {
    // A fetch begun after the request already answers it, whoever started it.
    state.owed.delete(hash);
    return;
  }
  if (query.state.fetchStatus !== "idle" || state.timers.has(hash)) {
    // In flight: the cache subscription drives it again when it settles. Or already scheduled.
    return;
  }
  // Clamped to the interval, so a wall clock stepped backwards never holds a refresh longer.
  const wait =
    started === undefined
      ? 0
      : Math.min(
          MIN_REFETCH_INTERVAL_MS,
          Math.max(0, started + MIN_REFETCH_INTERVAL_MS - Date.now())
        );
  if (wait > 0) {
    state.timers.set(
      hash,
      window.setTimeout(() => {
        state.timers.delete(hash);
        drive(state, queryClient, hash);
      }, wait)
    );
    return;
  }
  state.owed.delete(hash);
  // Unmounted while it waited: it is already marked stale, and fetches when it mounts again.
  if (!query.isActive() || query.isStatic()) {
    return;
  }
  void query.fetch(undefined, { cancelRefetch: false }).catch(() => undefined);
}

/**
 * Refreshes the queries `targets` reach, or, with no targets, every query the app holds but the
 * live-owned ones (the whole-cache refresh after a gap in the stream, or an event this build cannot
 * read), through the rules above.
 */
export function refreshQueries(queryClient: QueryClient, targets?: readonly RefreshTarget[]): void {
  const state = schedulerFor(queryClient);
  const cache = queryClient.getQueryCache();
  const requestedAt = Date.now();
  // One entry per query, however many targets reach it: `true` when one reaches it whatever its
  // data, else every predicate that does.
  const reached = new Map<Query, true | ((data: unknown) => boolean)[]>();
  if (targets === undefined) {
    for (const query of cache.getAll()) {
      reached.set(query, true);
    }
  } else {
    for (const target of targets) {
      for (const query of cache.findAll({ exact: target.exact, queryKey: target.queryKey })) {
        const current = reached.get(query);
        if (current === true) {
          continue;
        }
        reached.set(query, target.when === undefined ? true : [...(current ?? []), target.when]);
      }
    }
  }
  for (const [query, condition] of reached) {
    if (isLiveQuery(query)) {
      continue;
    }
    if (condition !== true) {
      if (query.state.data === undefined && query.state.fetchStatus !== "idle") {
        const pending = state.deferred.get(query.queryHash);
        state.deferred.set(query.queryHash, {
          requestedAt: Math.max(pending?.requestedAt ?? requestedAt, requestedAt),
          when: [...(pending?.when ?? []), ...condition],
        });
        continue;
      }
      if (!condition.some((when) => when(query.state.data))) {
        continue;
      }
    }
    owe(state, query, requestedAt);
    drive(state, queryClient, query.queryHash);
  }
}
