import type { QueryClient } from "@tanstack/react-query";

import type { Issue, IssueDetails } from "./types";

/**
 * Applies a PATCH response to the cached `IssueDetails` for its key. The response carries only
 * the `Issue` fields, so they are merged over the details-only arrays until the next full fetch.
 */
export function mergeIssue(queryClient: QueryClient, next: Issue): void {
  queryClient.setQueryData<IssueDetails>(["issue", next.key], (current) =>
    current === undefined ? undefined : { ...current, ...next }
  );
}
