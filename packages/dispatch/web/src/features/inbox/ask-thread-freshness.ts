interface FreshnessState {
  activeInboxFetches: Set<number>;
  latestInboxFetch: number;
  pending: Map<string, number>;
}

const stateByQueryClient = new WeakMap<object, FreshnessState>();

function stateFor(queryClient: object): FreshnessState {
  let state = stateByQueryClient.get(queryClient);
  if (state === undefined) {
    state = { activeInboxFetches: new Set(), latestInboxFetch: 0, pending: new Map() };
    stateByQueryClient.set(queryClient, state);
  }
  return state;
}

function discardEmptyState(queryClient: object, state: FreshnessState): void {
  if (state.activeInboxFetches.size === 0 && state.pending.size === 0) {
    stateByQueryClient.delete(queryClient);
  }
}

/** Starts an Inbox query generation. A marker records the generation that was in flight when
 * its event arrived, so only a later Inbox request can supersede that marker. */
export function beginInboxFetch(queryClient: object): number {
  const state = stateFor(queryClient);
  state.latestInboxFetch += 1;
  state.activeInboxFetches.add(state.latestInboxFetch);
  return state.latestInboxFetch;
}

/** Clears markers resolved by a newer Inbox response, or by a response that omits the ask. */
export function finishInboxFetch(
  queryClient: object,
  fetch: number,
  asks: readonly { readonly id: string }[] | undefined
): void {
  const state = stateFor(queryClient);
  state.activeInboxFetches.delete(fetch);
  const listed = asks === undefined ? undefined : new Set(asks.map((ask) => ask.id));
  for (const [askID, markedDuring] of state.pending) {
    if (markedDuring <= fetch && (markedDuring < fetch || !listed?.has(askID))) {
      state.pending.delete(askID);
    }
  }
  discardEmptyState(queryClient, state);
}

/** Records an ask event that arrived while the Inbox was fetching but before this thread existed
 * in the cache. Its card must read the targeted endpoint instead of trusting that in-flight list
 * response. */
export function markPendingAskThreadInvalidation(queryClient: object, askID: string): void {
  const state = stateFor(queryClient);
  state.pending.set(askID, state.latestInboxFetch);
}

/** The marker is read during render but cleared only after the targeted read has supplied data,
 * so a concurrent React render cannot consume it before the query starts. */
export function hasPendingAskThreadInvalidation(queryClient: object, askID: string): boolean {
  return stateByQueryClient.get(queryClient)?.pending.has(askID) ?? false;
}

export function clearPendingAskThreadInvalidation(queryClient: object, askID: string): void {
  const state = stateByQueryClient.get(queryClient);
  if (state === undefined) return;
  state.pending.delete(askID);
  discardEmptyState(queryClient, state);
}
