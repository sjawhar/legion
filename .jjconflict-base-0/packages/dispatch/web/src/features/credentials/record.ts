import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** One credential record by id, as `CredentialRecordPage` reads it. */
export const credentialRecordQuery = (recordId: string) =>
  queryOptions({
    queryKey: ["credential-record", recordId],
    queryFn: () => api.getCredentialRecord(recordId),
  });
