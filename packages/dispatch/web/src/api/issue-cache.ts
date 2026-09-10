import type { QueryClient } from "@tanstack/react-query";

import type { Issue, IssueDetails } from "./types";

/**
 * Applies a server `Issue` to the cached `IssueDetails` for its key. Mutation responses
 * (PATCH, make-primary) carry only the `Issue` fields, so they are merged over the details
 * rather than replacing them; the details-only arrays stay until the next full fetch.
 */
export function mergeIssue(queryClient: QueryClient, next: Issue): void {
  queryClient.setQueryData<IssueDetails>(["issue", next.key], (current) =>
    current === undefined ? undefined : { ...current, ...next }
  );
}
