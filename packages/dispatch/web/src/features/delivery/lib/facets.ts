// Facet types and the pure client-side filters the delivery timeline applies over one window's
// PRs. Unlike the prototype this ports from, `priority` and `component` are not client-side
// facets here: `DeliveryPR` carries no joined issue priority/components (LEGION-567's plan API
// keeps those server-side, since only the server has the Dispatch issue join), so the page sends
// `priority`/`component` to the server as query params (`client.ts`'s `DeliveryTimelineOptions`)
// and the response is already narrowed by them. Every other facet below is applied here, over the
// window's full (already priority/component-filtered) PR list, so picking one of them never
// re-fetches. No React, no fetch — components and tests call these directly.
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

/** The client-side facets: every query param `DeliveryTimelineOptions` accepts except `from`,
 *  `to`, `priority`, and `component`, which the page sends straight to the server instead of
 *  filtering locally (see the module comment above). */
export interface Filters {
  repo: string[];
  parentAgent: string[];
  session: string[];
  issue: string[];
  author: string[];
  rework: ReworkFacet[]; // both selected (or empty) = no restriction
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

function matchesFacet<T>(selected: T[], value: T): boolean {
  return selected.length === 0 || selected.includes(value);
}

function matchesFilters(filters: Filters, pr: DeliveryPR, skip?: keyof Filters): boolean {
  const reworkValue: ReworkFacet = pr.rework ? "rework" : "value";

  if (skip !== "repo" && !matchesFacet(filters.repo, pr.repo)) return false;
  if (skip !== "parentAgent" && filters.parentAgent.length > 0) {
    const placeholder = parentAgentPlaceholder(pr);
    const matchesReal = pr.parent_agent !== null && filters.parentAgent.includes(pr.parent_agent);
    const matchesPlaceholder =
      pr.parent_agent === null && filters.parentAgent.includes(placeholder);
    if (!matchesReal && !matchesPlaceholder) return false;
  }
  if (
    skip !== "session" &&
    filters.session.length > 0 &&
    !pr.sessions.some((s) => filters.session.includes(s))
  )
    return false;
  if (skip !== "issue" && filters.issue.length > 0) {
    const wantNoIssue = filters.issue.includes(NO_ISSUE);
    const matches = pr.issue !== null ? filters.issue.includes(pr.issue) : wantNoIssue;
    if (!matches) return false;
  }
  if (skip !== "author" && !matchesFacet(filters.author, pr.author)) return false;
  if (skip !== "rework" && !matchesFacet(filters.rework, reworkValue)) return false;
  if (skip !== "deployed" && !matchesFacet(filters.deployed, pr.deployed_status)) return false;
  if (skip !== "search" && filters.search.trim() !== "") {
    const needle = filters.search.trim().toLowerCase();
    if (!pr.title.toLowerCase().includes(needle) && !pr.id.toLowerCase().includes(needle))
      return false;
  }
  return true;
}

export function filterPRs(prs: readonly DeliveryPR[], filters: Filters): DeliveryPR[] {
  return prs.filter((pr) => matchesFilters(filters, pr));
}

/** Counts of PRs per value of a single facet, with every OTHER active client-side facet applied
 *  but this facet's own selection ignored — standard faceted-search counts, so picking a value
 *  never zeroes out its own count. */
export function facetCounts(
  prs: readonly DeliveryPR[],
  filters: Filters,
  facet: keyof Filters
): Record<string, number> {
  const matching = prs.filter((pr) => matchesFilters(filters, pr, facet));
  const counts: Record<string, number> = {};
  const bump = (key: string) => {
    counts[key] = (counts[key] ?? 0) + 1;
  };
  for (const pr of matching) {
    switch (facet) {
      case "repo":
        bump(pr.repo);
        break;
      case "parentAgent":
        bump(pr.parent_agent ?? parentAgentPlaceholder(pr));
        break;
      case "session":
        for (const s of pr.sessions) bump(s);
        break;
      case "issue":
        bump(pr.issue ?? NO_ISSUE);
        break;
      case "author":
        bump(pr.author);
        break;
      case "rework":
        bump(pr.rework ? "rework" : "value");
        break;
      case "deployed":
        bump(pr.deployed_status);
        break;
      default:
        break;
    }
  }
  return counts;
}
