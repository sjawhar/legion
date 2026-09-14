import type { QueryClient } from "@tanstack/react-query";

import type { Artifact, Issue, IssueDetails } from "./types";

/** The issue's primary document (its spec), or undefined until the details have loaded. */
export function primarySpec(
  issue: Pick<IssueDetails, "artifacts" | "primary_artifact_id"> | undefined
): Artifact | undefined {
  return issue?.artifacts.find((artifact) => artifact.id === issue.primary_artifact_id);
}

/**
 * Applies a PATCH response to the cached `IssueDetails` for its key. The response carries only
 * the `Issue` fields, so they are merged over the details-only arrays until the next full fetch.
 */
export function mergeIssue(queryClient: QueryClient, next: Issue): void {
  queryClient.setQueryData<IssueDetails>(["issue", next.key], (current) =>
    current === undefined ? undefined : { ...current, ...next }
  );
}
