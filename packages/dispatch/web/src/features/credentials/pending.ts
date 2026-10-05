import { type Query, type QueryClient, queryOptions, useQuery } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { CredentialPendingResponse, CredentialPendingRow } from "../../api/types";

/** How long a credential request may sit unseen on an open tab before the poll below notices it:
 *  short enough that an approver waiting on it never feels the wait, long enough that one GET a
 *  tick, per open tab, is unremarkable broker load (`useAgents.ts` polls its own list at the same
 *  cadence, for the same reason, but the two are independent constants - nothing ties them). */
const CREDENTIAL_POLL_INTERVAL_MS = 15_000;

/** `null` is a Dispatch with no secrets broker; anything else - even an empty list - is one that
 *  answered. `enabled` and `refetchInterval` below both gate on this one fact through this one
 *  function, so the two can never drift apart: a `null`-holding query can neither fetch again on
 *  its own nor have the poll armed for it. */
function holdsList(query: Query<CredentialPendingResponse | null>): boolean {
  return query.state.data !== null;
}

/** Every credential request waiting on the viewer, as the Inbox's credential-requests section
 *  lists them; `null` is a Dispatch with no secrets broker. The server reads its broker setting
 *  once, at boot, so the query holds that answer until the page reloads or the event stream
 *  reconnects (`reaskHeldCredentialList`): it is never fetched again while it holds `null` - not
 *  when a page mounts another reader, the window regains focus, or the event stream refreshes
 *  every query - so a deployment without a broker asks once per page load and once per
 *  reconnect. Once it holds a list, it also asks again every `CREDENTIAL_POLL_INTERVAL_MS` while
 *  the page is visible: TanStack's interval timer keeps ticking on a hidden tab, but leaves its
 *  fetch a no-op there unless `refetchIntervalInBackground` is set, which this query leaves
 *  unset - since no Dispatch event tells the page a request arrived at or left the broker. */
export const credentialPendingQuery = () =>
  queryOptions({
    enabled: holdsList,
    queryKey: ["credential-pending"],
    queryFn: () => api.getCredentialPending(),
    refetchInterval: (query) => (holdsList(query) ? CREDENTIAL_POLL_INTERVAL_MS : false),
  });

/** A reconnect of the event stream is the one moment the server may have restarted, perhaps
 *  with a broker configured, so it asks a held `null` again. The fetch runs over the held answer,
 *  so the list stays listed and empty while it is out and nothing blinks; a list that comes back
 *  makes the query an ordinary one that every refresh refetches. */
export function reaskHeldCredentialList(queryClient: QueryClient): void {
  const query = credentialPendingQuery();
  if (queryClient.getQueryData(query.queryKey) !== null) return;
  void queryClient.prefetchQuery({ ...query, staleTime: 0 });
}

/** What the Inbox knows about the credential requests waiting on the viewer. `listed` carries the
 *  list, empty where the broker is not configured (a deployment without one has none); while the
 *  list is `loading`, `requests` is empty and the Inbox claims nothing about them. `failed` means
 *  no list has ever loaded; a poll that fails after a list already loaded keeps reporting that
 *  list as `listed` rather than blanking it, since TanStack keeps the last good `data` on a
 *  refetch error and only clears it on the query's first, still-`isLoadingError`, attempt. */
export interface CredentialRequests {
  requests: readonly CredentialPendingRow[];
  status: "failed" | "listed" | "loading";
}

/** The one reading of the credential-request list. The section that lists the requests, the
 *  Inbox's empty state and every `Needs you` count read it here, so none of them can count a
 *  request another does not show. */
export function useCredentialRequests(): CredentialRequests {
  const pending = useQuery(credentialPendingQuery());
  if (pending.isPending) return { requests: [], status: "loading" };
  if (pending.isLoadingError) return { requests: [], status: "failed" };
  return { requests: pending.data?.pending ?? [], status: "listed" };
}
