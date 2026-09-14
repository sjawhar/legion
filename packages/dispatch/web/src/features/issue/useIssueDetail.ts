import { useQuery } from "@tanstack/react-query";
import { useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { type IssueRoute, isPrimaryDocumentArtifactRoute, issueTabForRoute } from "../refs/routes";
import { firstHighlightTerm } from "../search/search-model";

export function useIssueDetail(route: IssueRoute) {
  const { search } = useLocation();
  const artifactRoute = route.kind === "artifact" ? route : undefined;
  const artifactRouteSlug = artifactRoute?.slug;
  const query = new URLSearchParams(search);
  const commentId = query.get("comment") ?? undefined;
  const askId = query.get("ask") ?? undefined;
  const highlightTerm = firstHighlightTerm(query.get("q") ?? "");
  const issue = useQuery({
    queryKey: ["issue", route.key],
    queryFn: () => api.getIssue(route.key),
  });
  const state = useQuery({ queryKey: ["user-state"], queryFn: () => api.getMyState() });
  const primaryArtifact = primarySpec(issue.data);
  const selectedArtifact =
    artifactRouteSlug === undefined
      ? primaryArtifact
      : issue.data?.artifacts?.find(({ slug }) => slug === artifactRouteSlug);
  const isPrimaryArtifactRoute = isPrimaryDocumentArtifactRoute(route, selectedArtifact);
  const activeTab = issueTabForRoute(route, selectedArtifact);
  const conversationFocusItemId =
    route.kind === "ask" || route.kind === "comment" || route.kind === "message"
      ? route.id
      : undefined;

  return {
    activeTab,
    artifactRoute,
    askId,
    commentId,
    conversationFocusItemId,
    isPrimaryArtifactRoute,
    highlightTerm,
    issue,
    primaryArtifact,
    selectedArtifact,
    state,
  };
}
