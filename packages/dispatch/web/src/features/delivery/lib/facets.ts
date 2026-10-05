// Facet types for the delivery timeline. Every facet `DeliveryTimelineOptions` accepts — repo,
// parentAgent/session, issue, author, rework, deployed, priority, and component — is applied
// server-side now (`GET /api/v1/delivery/timeline`'s `filterDeliveryPullRequests` and its sibling
// matchers in `packages/envoy/internal/dispatch/api/delivery.go`), so `query.data.prs` IS the
// filtered list and this module carries no filtering logic of its own, only the shapes and labels
// the page and `FacetPanel` share: the `Filters` type DeliveryPage's URL state and FacetPanel's
// props use, the placeholder values/labels a PR with no resolved issue, session, or parent agent
// reads as, and `shortRepoLabel`'s display-only repo shortening. No React, no fetch.
import type { DeliveryPR } from "../../../api/types";

export type ReworkFacet = "value" | "rework";
export type DeployedFacet = DeliveryPR["deployed_status"];

// Placeholder facet values, split by cause instead of one catch-all "(none)": a PR that links no
// Dispatch issue at all reads "No issue", distinct from a PR with no `sessions` at all ("No
// session") versus one whose session(s) resolved to no parent-agent title ("Unknown agent").
// Same wording everywhere these show: Timeline lanes, FacetPanel's lists, and DrillDown.
export const NO_ISSUE = "__no_issue__";
export const NO_SESSION = "__no_session__";
export const NO_AGENT = "__no_agent__";

export const PLACEHOLDER_LABELS: Record<string, string> = {
  [NO_ISSUE]: "No issue",
  [NO_SESSION]: "No session",
  [NO_AGENT]: "Unknown agent",
};

/** The parent-agent placeholder for a PR with no resolved `parent_agent`. */
export function parentAgentPlaceholder(pr: DeliveryPR): string {
  return pr.sessions.length > 0 ? NO_AGENT : NO_SESSION;
}

export const DEPLOYED_LABELS: Record<DeployedFacet, string> = {
  deployed: "deployed",
  waiting: "waiting",
  not_tracked: "not tracked",
};

export const REWORK_LABELS: Record<ReworkFacet, string> = {
  value: "value",
  rework: "rework",
};

/** Drops the "owner/" prefix from a repo label when the bare repo name is unique among
 *  `allRepoKeys` — e.g. "widgets" instead of "acme/widgets" — but keeps the full "owner/repo"
 *  wherever two different owners share a repo name, so labels stay unambiguous. */
export function shortRepoLabel(allRepoKeys: string[], repo: string): string {
  const name = repo.split("/")[1] ?? repo;
  const owners = new Set(
    allRepoKeys.filter((key) => key.split("/")[1] === name).map((key) => key.split("/")[0])
  );
  return owners.size > 1 ? repo : name;
}

/** The delivery timeline's facets, every one of them applied server-side (see the module comment
 *  above): the page sends every active field as a query param, and the response is already
 *  narrowed by all of them. `search` stays client-side — the server never implements free-text
 *  search over title/PR id. */
export interface Filters {
  repo: string[];
  parentAgent: string[];
  session: string[];
  issue: string[];
  author: string[];
  rework: ReworkFacet[];
  deployed: DeployedFacet[];
  search: string;
}

export function emptyFilters(): Filters {
  return {
    repo: [],
    parentAgent: [],
    session: [],
    issue: [],
    author: [],
    rework: [],
    deployed: [],
    search: "",
  };
}
