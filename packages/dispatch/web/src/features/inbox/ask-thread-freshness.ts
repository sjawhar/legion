const pendingByQueryClient = new WeakMap<object, Set<string>>();

/** Records an ask event that arrived while the Inbox was fetching but before this thread existed
 * in the cache. Its card must read the targeted endpoint instead of trusting that in-flight list
 * response. */
export function markPendingAskThreadInvalidation(queryClient: object, askID: string): void {
  let pending = pendingByQueryClient.get(queryClient);
  if (pending === undefined) {
    pending = new Set();
    pendingByQueryClient.set(queryClient, pending);
  }
  pending.add(askID);
}

/** The marker is read during render but cleared only after the targeted read has supplied data,
 * so a concurrent React render cannot consume it before the query starts. */
export function hasPendingAskThreadInvalidation(queryClient: object, askID: string): boolean {
  return pendingByQueryClient.get(queryClient)?.has(askID) ?? false;
}

export function clearPendingAskThreadInvalidation(queryClient: object, askID: string): void {
  const pending = pendingByQueryClient.get(queryClient);
  if (pending === undefined) return;
  pending.delete(askID);
  if (pending.size === 0) pendingByQueryClient.delete(queryClient);
}

/** An Inbox response can only leave pending markers for rows it still contains. This clears
 * asks that resolved while the list was in flight, keeping the per-client marker set bounded. */
export function retainPendingAskThreadInvalidations(
  queryClient: object,
  askIDs: ReadonlySet<string>
): void {
  const pending = pendingByQueryClient.get(queryClient);
  if (pending === undefined) return;
  for (const askID of pending) {
    if (!askIDs.has(askID)) pending.delete(askID);
  }
  if (pending.size === 0) pendingByQueryClient.delete(queryClient);
}
