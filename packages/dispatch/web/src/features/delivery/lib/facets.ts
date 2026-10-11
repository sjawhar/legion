// The delivery timeline's facets, as the page's URL and FacetPanel name them. Every facet, and the
// search, is applied server-side (`GET /api/v1/delivery/timeline`, `api/delivery.go`), which also
// answers each facet's counts, so this module holds only the shapes and labels the page shares:
// the `Filters` the URL carries, the query parameter each facet is sent as, the placeholder values
// a PR with no issue, priority, component or session reads as, and the display-only repository
// shortening. No React, no fetch.
import type { DeliveryFacet, DeliveryPR } from "../../../api/types";

export type ReworkFacet = "value" | "rework";
export type DeployedFacet = DeliveryPR["deployed_status"];

// Placeholder facet values, split by cause instead of one catch-all "(none)": a PR that links no
// Dispatch issue reads "No issue", distinct from one whose issue has no priority or no component;
// a PR whose commits name no session reads "No session". The server counts and selects by the same
// four values. Same wording everywhere these show: Timeline lanes, FacetPanel's lists, PRList and
// DrillDown.
export const NO_ISSUE = "__no_issue__";
export const NO_PRIORITY = "__no_priority__";
export const NO_COMPONENT = "__no_component__";
export const NO_SESSION = "__no_session__";

export const PLACEHOLDER_LABELS: Record<string, string> = {
  [NO_ISSUE]: "No issue",
  [NO_PRIORITY]: "No priority",
  [NO_COMPONENT]: "No component",
  [NO_SESSION]: "No session",
};

export const DEPLOYED_LABELS: Record<DeployedFacet, string> = {
  deployed: "deployed",
  waiting: "waiting",
  not_tracked: "not tracked",
};

export const REWORK_LABELS: Record<ReworkFacet, string> = { value: "value", rework: "rework" };

/** Drops the "owner/" prefix from a repo label when the bare repo name is unique among
 *  `allRepoKeys` — e.g. "widgets" instead of "acme/widgets" — but keeps the full "owner/repo"
 *  wherever two different owners share a repo name, so labels stay unambiguous. */
export function shortRepoLabel(allRepoKeys: readonly string[], repo: string): string {
  const slash = repo.indexOf("/");
  if (slash === -1) return repo;
  const bare = repo.slice(slash + 1);
  const collides = allRepoKeys.some(
    (other) => other !== repo && other.slice(other.indexOf("/") + 1) === bare
  );
  return collides ? repo : bare;
}

/** The page's facet selections and search, as its URL carries them; empty selects everything. */
export interface Filters {
  repo: string[];
  parentAgent: string[];
  session: string[];
  issue: string[];
  priority: string[];
  /** `<project>/<id>`; a selected parent includes its descendants. */
  component: string[];
  author: string[];
  rework: ReworkFacet[];
  deployed: DeployedFacet[];
  search: string;
}

export type FacetKey = Exclude<keyof Filters, "search">;

/** Each facet's query parameter on the timeline read, which is also its `facet_counts` key. */
export const FACET_PARAMS: Record<FacetKey, DeliveryFacet> = {
  repo: "repo",
  parentAgent: "parent_agent",
  session: "session",
  issue: "issue",
  priority: "priority",
  component: "component",
  author: "author",
  rework: "rework",
  deployed: "deployed",
};

export const FACET_KEYS = Object.keys(FACET_PARAMS) as FacetKey[];
