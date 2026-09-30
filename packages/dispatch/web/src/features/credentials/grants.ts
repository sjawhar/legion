import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** Every live grant the viewer may revoke, as `GrantsSection` lists them. */
export const credentialGrantsQuery = () =>
  queryOptions({
    queryKey: ["credential-grants"],
    queryFn: () => api.getCredentialGrants(),
  });
