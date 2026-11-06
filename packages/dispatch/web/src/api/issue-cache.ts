import type { QueryClient } from "@tanstack/react-query";

import type { Artifact, DocumentRole, Issue, IssueDetails } from "./types";

/** The issue's primary document (its spec), or undefined until the details have loaded. */
export function primarySpec(
  issue: Pick<IssueDetails, "artifacts" | "primary_artifact_id"> | undefined
): Artifact | undefined {
  return issue?.artifacts.find((artifact) => artifact.id === issue.primary_artifact_id);
}

/** The issue's document in `role` - `verification.md` or `plan.md`, which the issue names in
 *  `verification_artifact_id` and `plan_artifact_id` - or undefined while the details are
 *  unread, when the issue holds none (`""`), and on a Dispatch older than document roles
 *  (absent). The id the issue names is the one answer, so a document whose name was changed by
 *  hand cannot put a second document on the tab. */
export function roleDocument(
  issue:
    | Pick<IssueDetails, "artifacts" | "plan_artifact_id" | "verification_artifact_id">
    | undefined,
  role: DocumentRole
): Artifact | undefined {
  const id = role === "plan" ? issue?.plan_artifact_id : issue?.verification_artifact_id;
  return id === undefined || id === ""
    ? undefined
    : issue?.artifacts.find((artifact) => artifact.id === id);
}

/**
 * Applies a PATCH response to the cached `IssueDetails` for its key. The response carries only
 * the `Issue` fields, so they are merged over the details-only fields until the next full fetch.
 * `route_status` is one of those, judged for the route the details were read with: a new route
 * clears it, its holder and its reason rather than wear the old route's, and the issue's own
 * event refetches them.
 */
export function mergeIssue(queryClient: QueryClient, next: Issue): void {
  queryClient.setQueryData<IssueDetails>(["issue", next.key], (current) => {
    if (current === undefined) {
      return undefined;
    }
    const reach =
      current.route === next.route
        ? {}
        : { route_status: null, route_holder: null, route_status_reason: null };
    return { ...current, ...next, ...reach };
  });
}
