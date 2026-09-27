import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** Every credential request waiting on the viewer, as the Inbox's credential-requests section
 *  lists them; a 404 FEATURE_OFF (the broker isn't configured) is the one query error the
 *  section hides silently rather than surfacing. */
export const credentialPendingQuery = () =>
  queryOptions({
    queryKey: ["credential-pending"],
    queryFn: () => api.getCredentialPending(),
  });
