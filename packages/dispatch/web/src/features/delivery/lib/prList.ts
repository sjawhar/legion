// Row model for the PR list view: display labels and sort keys per PR, computed once per
// population so the table's cells and sort functions read plain fields. Pure: no React.
import type { DeliveryPR } from "../../../api/types";
import {
  DEPLOYED_LABELS,
  NO_ISSUE,
  NO_PRIORITY,
  NO_SESSION,
  PLACEHOLDER_LABELS,
  shortRepoLabel,
} from "./facets";

const PRIORITY_RANK: Record<NonNullable<DeliveryPR["priority"]>, number> = {
  P0: 0,
  P1: 1,
  P2: 2,
  P3: 3,
};
const NO_PRIORITY_RANK = 4;

/** Issue sort order: by priority (P0 first, "No priority" after P3), then by Dispatch key. */
export interface IssueSortKey {
  priorityRank: number;
  key: string;
}

export interface PRListRow {
  pr: DeliveryPR;
  mergedMs: number | undefined;
  /** Short repo label plus number, e.g. "widgets#123" (see shortRepoLabel). */
  prLabel: string;
  /** "ACME-100 · P1", "ACME-100 · No priority", or "No issue". */
  issueLabel: string;
  /** undefined for a PR with no issue, so it sorts last in either direction. */
  issueSort: IssueSortKey | undefined;
  /** The parent agent's title, or "No session". */
  parentAgentLabel: string;
  /** undefined when parentAgentLabel is the placeholder, so it sorts last. */
  parentAgentSort: string | undefined;
  /** additions + deletions; undefined while the row is partial. */
  size: number | undefined;
  deployedLabel: string;
  deployedMs: number | undefined;
  /** Merge-to-production lead time in minutes; undefined while not deployed. */
  leadTimeMinutes: number | undefined;
}

/** A PR's lead time: merge to production, in minutes; undefined while it isn't deployed. */
export function leadTimeMinutes(pr: DeliveryPR): number | undefined {
  if (pr.merged_at === null || pr.deployed_at === null) return undefined;
  return (Date.parse(pr.deployed_at) - Date.parse(pr.merged_at)) / 60_000;
}

/** Minutes as the page shows durations: "45m", "3.5h", "2.1d". */
export function formatMinutes(minutes: number): string {
  if (minutes < 60) return `${Math.round(minutes)}m`;
  if (minutes < 1440) return `${(minutes / 60).toFixed(1)}h`;
  return `${(minutes / 1440).toFixed(1)}d`;
}

export function buildPRListRows(
  prs: readonly DeliveryPR[],
  allRepos: readonly string[]
): PRListRow[] {
  return prs.map((pr) => ({
    pr,
    mergedMs: pr.merged_at === null ? undefined : Date.parse(pr.merged_at),
    prLabel: `${shortRepoLabel(allRepos, pr.repo)}#${pr.number}`,
    issueLabel:
      pr.issue === null
        ? PLACEHOLDER_LABELS[NO_ISSUE]
        : `${pr.issue} \u00b7 ${pr.priority ?? PLACEHOLDER_LABELS[NO_PRIORITY]}`,
    issueSort:
      pr.issue === null
        ? undefined
        : {
            priorityRank: pr.priority === null ? NO_PRIORITY_RANK : PRIORITY_RANK[pr.priority],
            key: pr.issue,
          },
    parentAgentLabel: pr.parent_agent ?? PLACEHOLDER_LABELS[NO_SESSION],
    parentAgentSort: pr.parent_agent ?? undefined,
    size: pr.additions === null || pr.deletions === null ? undefined : pr.additions + pr.deletions,
    deployedLabel: DEPLOYED_LABELS[pr.deployed_status],
    deployedMs: pr.deployed_at === null ? undefined : Date.parse(pr.deployed_at),
    leadTimeMinutes: leadTimeMinutes(pr),
  }));
}

/** Ascending comparator for IssueSortKey: priority rank, then Dispatch key with numeric runs
 *  compared as numbers ("ACME-99" before "ACME-100"). */
export function compareIssueSort(a: IssueSortKey, b: IssueSortKey): number {
  return (
    a.priorityRank - b.priorityRank || a.key.localeCompare(b.key, undefined, { numeric: true })
  );
}

/** Every body row of the list is this tall, so the rows on screen are arithmetic, with no
 *  measuring. The sticky header row is the same height. */
export const ROW_HEIGHT_PX = 36;
/** Rows rendered past each edge of the scrolled-to window. */
export const OVERSCAN_ROWS = 12;

/** The rows `[first, last)` of `rowCount` the list renders when scrolled to `scrollTop` in a
 *  viewport `viewportHeight` tall: those on screen plus OVERSCAN_ROWS each side. */
export function visibleRowRange(
  scrollTop: number,
  viewportHeight: number,
  rowCount: number
): { first: number; last: number } {
  const first = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT_PX) - OVERSCAN_ROWS);
  const last = Math.min(
    rowCount,
    Math.ceil((scrollTop + viewportHeight) / ROW_HEIGHT_PX) + OVERSCAN_ROWS
  );
  return { first, last };
}

/** The scroll offset that keeps row `index` fully in view below the sticky header, moving as
 *  little as possible from `scrollTop`. */
export function scrollTopToShowRow(
  index: number,
  scrollTop: number,
  viewportHeight: number
): number {
  const rowTop = index * ROW_HEIGHT_PX;
  const bodyHeight = Math.max(ROW_HEIGHT_PX, viewportHeight - ROW_HEIGHT_PX);
  if (rowTop < scrollTop) return rowTop;
  if (rowTop + ROW_HEIGHT_PX > scrollTop + bodyHeight) return rowTop + ROW_HEIGHT_PX - bodyHeight;
  return scrollTop;
}
