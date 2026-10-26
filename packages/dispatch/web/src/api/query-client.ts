import {
  notifyManager,
  type Query,
  QueryClient,
  type QueryKey,
  type RefetchOptions,
  type RefetchQueryFilters,
} from "@tanstack/react-query";

/**
 * Dispatch's query client: TanStack's, except that no read ever has two requests in flight.
 * `refetchQueries` - and through it `invalidateQueries` and `resetQueries`, which TanStack routes
 * through this method - never cancels a running request. A query that is already fetching is
 * fetched once more when that fetch settles, so an invalidation a mutation makes mid-fetch still
 * ends on the answer composed after the write, and the first request is not left running on the
 * server with its answer thrown away. Calls for one query while it waits share that one follow-up
 * fetch. Every mutation's own `invalidateQueries` call keeps its shape.
 */
export class DispatchQueryClient extends QueryClient {
  readonly #followUps = new Map<string, Promise<void>>();

  override refetchQueries<TTaggedQueryKey extends QueryKey = QueryKey>(
    filters?: RefetchQueryFilters<TTaggedQueryKey>,
    options: RefetchOptions = {}
  ): Promise<void> {
    const promises = notifyManager.batch(() =>
      this.getQueryCache()
        .findAll(filters)
        .filter((query) => !query.isDisabled() && !query.isStatic())
        .map((query) => this.#refetch(query, options))
    );
    return Promise.all(promises).then(() => undefined);
  }

  #refetch(query: Query, options: RefetchOptions): Promise<void> {
    const running = query.state.fetchStatus === "idle" ? undefined : query.promise;
    if (running === undefined) {
      const fetched = query.fetch(undefined, { ...options, cancelRefetch: false });
      return (options.throwOnError ? fetched : fetched.catch(() => undefined)).then(
        () => undefined
      );
    }
    const hash = query.queryHash;
    let followUp = this.#followUps.get(hash);
    if (followUp === undefined) {
      followUp = running
        .then(
          () => undefined,
          () => undefined
        )
        .then(() => {
          this.#followUps.delete(hash);
          // Removed while it waited (a sign-out, garbage collection): nothing renders it now.
          if (this.getQueryCache().get(hash) !== query || query.isDisabled() || query.isStatic()) {
            return;
          }
          // A fetch that began after the one waited on was composed after this call: it answers.
          const newer = query.state.fetchStatus === "idle" ? undefined : query.promise;
          if (newer !== undefined) {
            return newer.then(
              () => undefined,
              () => undefined
            );
          }
          return this.#refetch(query, options);
        });
      this.#followUps.set(hash, followUp);
    }
    // TanStack resolves a paused fetch's refetch at once rather than holding its caller until the
    // network returns; the follow-up still runs when it does.
    return query.state.fetchStatus === "paused" ? Promise.resolve() : followUp;
  }
}
