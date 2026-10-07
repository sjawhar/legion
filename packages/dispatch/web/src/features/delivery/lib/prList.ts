// Row model for the PR list view: display labels and sort keys per PR, computed once per
// population so the table's cells and sort functions read plain fields. Pure: no React. Unlike
// the prototype this ports from, there is no `lib/dora.ts` in this slice (the DORA/measures panel
// is explicitly out of LEGION-567 slice 1's scope, per the plan's "Order"), so lead time is a
// small inline merge-to-production computation rather than an import from a module this slice
// does not ship.
import type { DeliveryPR } from "../../../api/types";
import {
  DEPLOYED_LABELS,
  NO_ISSUE,
  PLACEHOLDER_LABELS,
  parentAgentPlaceholder,
  shortRepoLabel,
} from "./facets";

export interface PRListRow {
  pr: DeliveryPR;
  /** `undefined` for a still-partial row with no `merged_at` yet. */
  mergedMs: number | undefined;
  /** Short repo label plus number, e.g. "widgets#123" (see shortRepoLabel). */
  prLabel: string;
  /** The Dispatch key, or the "No issue" placeholder. */
  issueLabel: string;
  /** `undefined` for a PR with no issue, so it sorts last in either direction. */
  issueSort: string | undefined;
  /** The resolved parent-agent title, or the placeholder label ("Unknown agent" / "No session"). */
  parentAgentLabel: string;
  /** `undefined` when parentAgentLabel is a placeholder, so placeholders sort last. */
  parentAgentSort: string | undefined;
  /** additions + deletions; `undefined` while the row is partial. */
  size: number | undefined;
  deployedLabel: string;
  deployedMs: number | undefined;
  /** Merge-to-production lead time in minutes; `undefined` while not deployed or still partial. */
  leadTimeMinutes: number | undefined;
}

function leadTimeMinutes(pr: DeliveryPR): number | undefined {
  if (pr.merged_at === null || pr.deployed_at === null) return undefined;
  return (Date.parse(pr.deployed_at) - Date.parse(pr.merged_at)) / 60_000;
}

export function buildPRListRows(prs: readonly DeliveryPR[]): PRListRow[] {
  const allRepos = Array.from(new Set(prs.map((pr) => pr.repo)));
  return prs.map((pr) => {
    const agentPlaceholder = parentAgentPlaceholder(pr);
    return {
      pr,
      mergedMs: pr.merged_at !== null ? Date.parse(pr.merged_at) : undefined,
      prLabel: `${shortRepoLabel(allRepos, pr.repo)}#${pr.number}`,
      issueLabel: pr.issue ?? PLACEHOLDER_LABELS[NO_ISSUE] ?? NO_ISSUE,
      issueSort: pr.issue ?? undefined,
      parentAgentLabel: pr.parent_agent ?? PLACEHOLDER_LABELS[agentPlaceholder] ?? agentPlaceholder,
      parentAgentSort: pr.parent_agent ?? undefined,
      size:
        pr.additions !== null && pr.deletions !== null ? pr.additions + pr.deletions : undefined,
      deployedLabel: DEPLOYED_LABELS[pr.deployed_status],
      deployedMs: pr.deployed_at !== null ? Date.parse(pr.deployed_at) : undefined,
      leadTimeMinutes: leadTimeMinutes(pr),
    };
  });
}
