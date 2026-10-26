import type { Query, QueryClient, QueryFilters } from "@tanstack/react-query";

import { ApiError } from "./client";

/** Whether a query is owned by Dispatch's live delivery socket rather than refreshed by events. */
export function isLiveQuery(query: Pick<Query, "meta">): boolean {
  return query.meta?.live === true;
}

/** Whether a query's last answer was a 404: a record that does not exist. */
export function answeredNotFound(query: { state: { error: Error | null } }): boolean {
  return query.state.error instanceof ApiError && query.state.error.status === 404;
}

/**
 * How a stream event refreshes the query cache: which of a batch's keys are worth invalidating,
 * and what to do about a query whose first response is still in flight when the event arrives.
 * `api/sse.ts` owns which keys an event names; this module owns what happens to them.
 */

/**
 * The key an event queues to refresh every issue detail the cache holds: each `["issue", key]`
 * query but one whose last answer was a 404. Such a query is mostly a key-shaped word a body writes
 * bare that names no issue (`UTF-8`, `useBareReferences`), and only a new issue can change that
 * answer, so every event but `issue.created` (an issue's update or close, a child's status, a
 * mapping write, a truncated count) asks nothing about it, where each would otherwise cost one
 * request per such word on screen; `issue.created`'s whole `["issue"]` and a whole-cache refresh
 * (the stream's reconnect) still ask. `coalescePrefixKeys` keeps `["issue"]` over it, and it over
 * a narrower issue key, when one batch queues both.
 */
export const existingIssuesKey = ["issue", "existing"] as const;

function isExistingIssuesKey(key: readonly unknown[]): boolean {
  return (
    key.length === existingIssuesKey.length &&
    key.every((part, index) => part === existingIssuesKey[index])
  );
}

/**
 * React Query matches by prefix, so a queued key that another queued key is a prefix of is the
 * same refresh twice: `invalidateQueries` defaults to `cancelRefetch: true`, so the second call
 * cancels the first's in-flight fetch and starts another request for the same query. Keeps the
 * broadest queued key of each family and drops the rest. The relation is element-wise and
 * strict, so a sibling family (`["artifact-ref"]` beside `["artifact"]`) is never swallowed and
 * the shortest queued key is never dropped. `existingIssuesKey` stands for the `["issue"]` family
 * here, so it drops a narrower issue key beside it (`["issue", "CORE-1"]`, the event's own issue,
 * which exists) and `["issue"]` drops it.
 */
export function coalescePrefixKeys(keys: readonly (readonly unknown[])[]): (readonly unknown[])[] {
  const existingIssues = keys.some(isExistingIssuesKey);
  return keys.filter(
    (key) =>
      !keys.some(
        (other) =>
          other.length < key.length && other.every((part, index) => Object.is(part, key[index]))
      ) && !(existingIssues && key[0] === "issue" && key.length > 1 && !isExistingIssuesKey(key))
  );
}

// Per QueryClient: the query hashes whose current first load has already been cancelled once,
// and those whose landing response owes a refresh. A hash leaves `cancelled` when its query
// holds data or is removed, and leaves `refreshOnSettle` when that query settles or is removed.
interface FirstLoadState {
  cancelled: Set<string>;
  refreshOnSettle: Set<string>;
}

const firstLoadState = new WeakMap<QueryClient, FirstLoadState>();

function stateFor(queryClient: QueryClient): FirstLoadState {
  let state = firstLoadState.get(queryClient);
  if (state === undefined) {
    const tracked: FirstLoadState = { cancelled: new Set(), refreshOnSettle: new Set() };
    state = tracked;
    firstLoadState.set(queryClient, tracked);
    // One subscription for the client, not one per hash, so nothing can be stranded. A removal
    // clears `cancelled` because a query removed and recreated between two flushes (sign-out
    // then sign-in, or garbage collection) would otherwise keep its hash and have the new
    // query's genuine first load skip its cancel. It clears `refreshOnSettle` only to keep that
    // set bounded: a stale entry there is harmless, since registering is an idempotent `add`
    // and the added notification consumes it before any observer attaches, so the replacement
    // query's first load costs no extra request.
    queryClient.getQueryCache().subscribe((change) => {
      const hash = change.query.queryHash;
      if (change.type === "removed") {
        tracked.cancelled.delete(hash);
        tracked.refreshOnSettle.delete(hash);
        return;
      }
      if (change.query.state.fetchStatus !== "idle" || !tracked.refreshOnSettle.delete(hash)) {
        return;
      }
      queryClient.invalidateQueries({ exact: true, queryKey: change.query.queryKey });
    });
  }
  return state;
}

/**
 * Cancels the first load of every active query matching `filters` (every query when they are
 * omitted), at most once per query until that query holds data.
 *
 * `invalidateQueries` refetches with `cancelRefetch`, whose whole job is to restart a request
 * that is already running, so the refetch cannot be satisfied by a body the server composed
 * before the event. TanStack applies it only to a query that already holds data
 * (`Query.fetch`): during a query's *first* load it joins the in-flight promise instead, and
 * that response's success clears `isInvalidated` and stamps `dataUpdatedAt`, leaving the
 * pre-event body fresh for the whole `staleTime`. That is the screen a list keeps showing when
 * an event arrives between its mount and its first response. Reverting that load puts the query
 * back into the pending state it is already rendering and leaves it idle, so the invalidation's
 * own refetch issues a new request.
 *
 * TanStack's gate is not arbitrary, though: it is what stops an invalidation from cancelling the
 * request that would have filled the screen. Cancelling on every flush starves a load slower
 * than the gap between events, so a query is cancelled at most once until it holds data; a
 * later flush lets the running request land and refreshes the query when it settles instead.
 * Progress is guaranteed and the post-event body still wins. Only active, non-static queries
 * are cancelled, which is a subset of the set `invalidateQueries` refetches. A live-owned query
 * (`meta.live`, the delivery reads `api/delivery-live.ts` keeps current) is never cancelled: its
 * first load is a subscription whose snapshot the live client writes itself, and no event of
 * this stream names it.
 *
 * The cancelled request is not aborted on the wire: TanStack aborts through the signal on the
 * query function's context, and no query function in this app reads it. So the window costs two
 * full round trips for each query it touches - the orphaned body the client discards, and the
 * post-event one it renders.
 */
function cancelFirstLoads(queryClient: QueryClient, filters?: readonly QueryFilters[]): void {
  const cache = queryClient.getQueryCache();
  const { cancelled, refreshOnSettle } = stateFor(queryClient);
  // A removed query left `cancelled` on the subscription above, so a hash still here names a
  // live query; it is released once that query holds data.
  for (const hash of cancelled) {
    if (cache.get(hash)?.state.data !== undefined) {
      cancelled.delete(hash);
    }
  }
  // One `findAll` per filter rather than one over the union of the batch's keys: the filters
  // differ per key, and a query two keys both match is reached twice, which the sets below absorb.
  for (const filter of filters ?? [{}]) {
    const loads = cache.findAll({
      fetchStatus: "fetching",
      predicate: (query) =>
        query.state.data === undefined &&
        !query.isStatic() &&
        !isLiveQuery(query) &&
        (filter.predicate?.(query) ?? true),
      queryKey: filter.queryKey,
      type: "active",
    });
    for (const load of loads) {
      const hash = load.queryHash;
      if (!cancelled.has(hash)) {
        cancelled.add(hash);
        load.cancel({ revert: true });
        continue;
      }
      // The running request is left to land, and the client's one subscriber refreshes this
      // query when it settles.
      refreshOnSettle.add(hash);
    }
  }
}

/** Refreshes `queryKeys` (each by prefix, `existingIssuesKey` as its own filter), or every
 *  query the app holds but the live-owned ones when they are omitted: a live query is kept
 *  current by its own socket, so a whole-cache refresh (the stream's reconnect, an unknown event)
 *  would only re-subscribe it and have Dispatch send its snapshot again. */
export function refreshQueries(
  queryClient: QueryClient,
  queryKeys?: readonly (readonly unknown[])[]
): void {
  // Each key by prefix, but the issues that exist: every issue query but one that answered 404.
  const filters = queryKeys?.map(
    (queryKey): QueryFilters =>
      isExistingIssuesKey(queryKey)
        ? { predicate: (query) => !answeredNotFound(query), queryKey: ["issue"] }
        : { queryKey }
  );
  // Every first load this batch touches is cancelled before any of it is refetched: two keys in
  // one batch can match the same query, and interleaving would have the second key cancel the
  // request the first one just started.
  cancelFirstLoads(queryClient, filters);
  if (filters === undefined) {
    queryClient.invalidateQueries({ predicate: (query) => !isLiveQuery(query) });
    return;
  }
  for (const filter of filters) {
    queryClient.invalidateQueries(filter);
  }
}
