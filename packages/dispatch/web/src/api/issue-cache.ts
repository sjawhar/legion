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
 * the `Issue` fields, so they are merged over the details-only fields until the next full fetch.
 * `route_status` is one of those, judged for the route the details were read with: a new route
 * clears it rather than wear the old route's status, and the issue's own event refetches it.
 */
export function mergeIssue(queryClient: QueryClient, next: Issue): void {
  queryClient.setQueryData<IssueDetails>(["issue", next.key], (current) => {
    if (current === undefined) {
      return undefined;
    }
    const reach = current.route === next.route ? {} : { route_status: null, route_holder: null };
    return { ...current, ...next, ...reach };
  });
}
