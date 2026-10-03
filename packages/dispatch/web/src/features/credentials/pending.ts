import { queryOptions, useQuery } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { CredentialPendingRow } from "../../api/types";

/** Every credential request waiting on the viewer, as the Inbox's credential-requests section
 *  lists them; `null` is a Dispatch with no secrets broker. The server reads its broker setting
 *  once, at boot, so that answer holds for the session: the query is never fetched again once it
 *  holds it - not when a page mounts another reader, the window regains focus, or the event
 *  stream refreshes every query - so a deployment without a broker asks once per page load. */
export const credentialPendingQuery = () =>
  queryOptions({
    enabled: (query) => query.state.data !== null,
    queryKey: ["credential-pending"],
    queryFn: () => api.getCredentialPending(),
  });

/** What the Inbox knows about the credential requests waiting on the viewer. `listed` carries the
 *  list, empty where the broker is not configured (a deployment without one has none); while the
 *  list is `loading` or after it `failed`, `requests` is empty and the Inbox claims nothing about
 *  them. */
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
  if (pending.isError) return { requests: [], status: "failed" };
  return { requests: pending.data?.pending ?? [], status: "listed" };
}
