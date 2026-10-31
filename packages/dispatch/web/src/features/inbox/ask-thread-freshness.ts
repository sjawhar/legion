interface FreshnessState {
  activeInboxFetches: Set<number>;
  latestInboxFetch: number;
  /** Ask id → the Inbox generation in flight when an event on that ask arrived. */
  pending: Map<string, number>;
  /** Issue key → the Inbox generation in flight when an event moving every thread on it arrived. */
  pendingIssues: Map<string, number>;
}

const stateByQueryClient = new WeakMap<object, FreshnessState>();

function stateFor(queryClient: object): FreshnessState {
  let state = stateByQueryClient.get(queryClient);
  if (state === undefined) {
    state = {
      activeInboxFetches: new Set(),
      latestInboxFetch: 0,
      pending: new Map(),
      pendingIssues: new Map(),
    };
    stateByQueryClient.set(queryClient, state);
  }
  return state;
}

function discardEmptyState(queryClient: object, state: FreshnessState): void {
  if (
    state.activeInboxFetches.size === 0 &&
    state.pending.size === 0 &&
    state.pendingIssues.size === 0
  ) {
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

/** Clears markers resolved by a newer Inbox response, or by a response that omits the ask. An
 * issue's marker clears only on a newer response that arrived: its body was composed after the
 * event, so every card it seeds on that issue holds the moved claimant and route. */
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
  if (asks !== undefined) {
    for (const [issueKey, markedDuring] of state.pendingIssues) {
      if (markedDuring < fetch) {
        state.pendingIssues.delete(issueKey);
      }
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

/** Records an event that moves every ask thread on an issue (a claim, a release, a route change)
 * and arrived while the Inbox was fetching. The in-flight body names the old claimant and route
 * for each of the issue's asks, so a card on that issue that mounts from it reads the targeted
 * endpoint instead, until a newer Inbox response lands. */
export function markPendingAskThreadsOnIssue(queryClient: object, issueKey: string): void {
  const state = stateFor(queryClient);
  state.pendingIssues.set(issueKey, state.latestInboxFetch);
}

/** The marker is read during render but cleared only after the targeted read has supplied data,
 * so a concurrent React render cannot consume it before the query starts. An ask on a marked issue
 * is pending too. */
export function hasPendingAskThreadInvalidation(
  queryClient: object,
  ask: { readonly id: string; readonly issue_key: string | null }
): boolean {
  const state = stateByQueryClient.get(queryClient);
  if (state === undefined) return false;
  return (
    state.pending.has(ask.id) || (ask.issue_key !== null && state.pendingIssues.has(ask.issue_key))
  );
}

/** Clears the ask's own marker once its targeted read has supplied data. An issue's marker stays:
 * other cards on the issue may still mount from the stale Inbox body, and only a newer Inbox
 * response clears it (`finishInboxFetch`). */
export function clearPendingAskThreadInvalidation(queryClient: object, askID: string): void {
  const state = stateByQueryClient.get(queryClient);
  if (state === undefined) return;
  state.pending.delete(askID);
  discardEmptyState(queryClient, state);
}
