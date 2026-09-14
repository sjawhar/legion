import type { IssueSummary, UpdateIssueInput } from "../../api/types";

export const issueStatuses = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;

export type IssueStatus = (typeof issueStatuses)[number];
export const openIssueStatuses = issueStatuses.filter((status) => status !== "done");

const statusLabels: Record<IssueStatus, string> = {
  triage: "Triage",
  icebox: "Icebox",
  backlog: "Backlog",
  todo: "Todo",
  in_progress: "In progress",
  testing: "Testing",
  needs_review: "Needs review",
  retro: "Retro",
  done: "Done",
};

export function statusLabel(status: IssueStatus): string {
  return statusLabels[status];
}

export interface BoardColumn {
  readonly status: IssueStatus;
  readonly issues: IssueSummary[];
}

/**
 * Splits the project list into lifecycle columns, keeping the list's order inside each one.
 * The server lists issues by status then rank, and an optimistic move places a card where it
 * was dropped before the server has re-ranked it, so the list order is the column order.
 */
export function groupIssuesByStatus(issues: readonly IssueSummary[]): BoardColumn[] {
  return issueStatuses.map((status) => ({
    status,
    issues: issues.filter((issue) => issue.status === status),
  }));
}

export function rankInputForInsertion(
  issues: readonly IssueSummary[],
  insertionIndex: number
): { before?: string; after?: string } {
  const before = issues[insertionIndex];
  const after = issues[insertionIndex - 1];
  return {
    ...(after === undefined ? {} : { after: after.key }),
    ...(before === undefined ? {} : { before: before.key }),
  };
}

export interface DropTarget {
  readonly status: IssueStatus;
  readonly insertionIndex: number;
}

/**
 * Translates a dnd-kit drop - `overId` is another card's key or a column's `status:<s>` id -
 * into the column and position `moveCard` takes. Within a column the card lands at the over
 * card's current position (dnd-kit's `arrayMove`: one step down lands below the card it
 * passed, one step up lands above it); a drop onto another column's card lands above that
 * card; a drop onto a column itself - its header, an empty column, or a collapsed rail -
 * appends. Undefined when the drop is not a move.
 */
export function dropTarget(
  issues: readonly IssueSummary[],
  activeKey: string,
  overId: string
): DropTarget | undefined {
  if (activeKey === overId) {
    return undefined;
  }
  if (!issues.some((issue) => issue.key === activeKey)) {
    return undefined;
  }
  const overIssue = issues.find((issue) => issue.key === overId);
  if (overIssue !== undefined) {
    // The active card is in this column only when the move stays inside it, which is exactly
    // when its own position must count (dnd-kit's arrayMove).
    const status = overIssue.status as IssueStatus;
    const column = issues.filter((issue) => issue.status === status);
    return { status, insertionIndex: column.findIndex((issue) => issue.key === overId) };
  }
  const status = overId.replace(/^status:/, "");
  if (!issueStatuses.includes(status as IssueStatus)) {
    return undefined;
  }
  return {
    status: status as IssueStatus,
    insertionIndex: issues.filter((issue) => issue.status === status && issue.key !== activeKey)
      .length,
  };
}

/**
 * Places `key` at `insertionIndex` of the `targetStatus` column: the optimistic list to show
 * while the server answers, and the PATCH body naming the visible neighbours (`status` only
 * when the column changes). Undefined when the card is unknown or would not move.
 */
export function moveIssue(
  issues: readonly IssueSummary[],
  key: string,
  targetStatus: IssueStatus,
  insertionIndex: number
): { issues: IssueSummary[]; input: UpdateIssueInput } | undefined {
  const active = issues.find((issue) => issue.key === key);
  if (active === undefined) {
    return undefined;
  }
  const columns = groupIssuesByStatus(issues.filter((issue) => issue.key !== key));
  const target = columns.find((column) => column.status === targetStatus);
  if (target === undefined) {
    return undefined;
  }
  const index = Math.min(Math.max(insertionIndex, 0), target.issues.length);
  const rank = rankInputForInsertion(target.issues, index);
  target.issues.splice(index, 0, { ...active, status: targetStatus });
  const moved = columns.flatMap((column) => column.issues);
  if (
    active.status === targetStatus &&
    moved.every((issue, position) => issue.key === issues[position]?.key)
  ) {
    return undefined;
  }
  return {
    issues: moved,
    input: { ...(active.status === targetStatus ? {} : { status: targetStatus }), rank },
  };
}
