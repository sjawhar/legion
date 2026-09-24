import type { QueryClient } from "@tanstack/react-query";

/**
 * How a stream event refreshes the query cache: which of a batch's keys are worth invalidating,
 * and what to do about a query whose first response is still in flight when the event arrives.
 * `api/sse.ts` owns which keys an event names; this module owns what happens to them.
 */

/**
 * React Query matches by prefix, so a queued key that another queued key is a prefix of is the
 * same refresh twice: `invalidateQueries` defaults to `cancelRefetch: true`, so the second call
 * cancels the first's in-flight fetch and starts another request for the same query. Keeps the
 * broadest queued key of each family and drops the rest. The relation is element-wise and
 * strict, so a sibling family (`["artifact-ref"]` beside `["artifact"]`) is never swallowed and
 * the shortest queued key is never dropped.
 */
export function coalescePrefixKeys(keys: readonly (readonly unknown[])[]): (readonly unknown[])[] {
  return keys.filter(
    (key) =>
      !keys.some(
        (other) =>
          other.length < key.length && other.every((part, index) => Object.is(part, key[index]))
      )
  );
}

// Per QueryClient: the query hashes whose current first load has already been cancelled once,
// and those whose landing response owes a refresh. Both are cleared as each query settles.
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
    // One subscription for the client, not one per hash: it cannot be stranded, and a removal
    // clears both sets. `cancelled` needs the removal because a query removed and recreated
    // between two flushes (sign-out then sign-in, or garbage collection) would otherwise keep
    // its hash and have the new query's genuine first load skip its cancel; `refreshOnSettle`
    // needs it because `QueryCache.remove` cancels silently, so a removed query never reaches
    // `"idle"` and a refresh owed to it would otherwise be owed forever.
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
 * Cancels the first load of every active query matching `queryKeys` (every query when they are
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
 * are cancelled, which is a subset of the set `invalidateQueries` refetches.
 *
 * The cancelled request is not aborted on the wire: TanStack aborts through the signal on the
 * query function's context, and no query function in this app reads it. So the window costs two
 * full round trips for each query it touches - the orphaned body the client discards, and the
 * post-event one it renders.
 */
function cancelFirstLoads(
  queryClient: QueryClient,
  queryKeys?: readonly (readonly unknown[])[]
): void {
  const cache = queryClient.getQueryCache();
  const { cancelled, refreshOnSettle } = stateFor(queryClient);
  for (const hash of cancelled) {
    const settled = cache.get(hash);
    if (settled === undefined || settled.state.data !== undefined) {
      cancelled.delete(hash);
    }
  }
  // One `findAll` per key rather than one over the union, so the sets below are reached once
  // per query per batch.
  for (const queryKey of queryKeys ?? [undefined]) {
    const loads = cache.findAll({
      fetchStatus: "fetching",
      predicate: (query) => query.state.data === undefined && !query.isStatic(),
      queryKey,
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

/** Refreshes `queryKeys`, or every query the app holds when they are omitted. */
export function refreshQueries(
  queryClient: QueryClient,
  queryKeys?: readonly (readonly unknown[])[]
): void {
  // Every first load this batch touches is cancelled before any of it is refetched: two keys in
  // one batch can match the same query, and interleaving would have the second key cancel the
  // request the first one just started.
  cancelFirstLoads(queryClient, queryKeys);
  // An explicitly `undefined` key matches every query, exactly as omitting the filter does.
  for (const queryKey of queryKeys ?? [undefined]) {
    queryClient.invalidateQueries({ queryKey });
  }
}
