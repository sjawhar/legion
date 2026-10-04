import { ISSUE_STATUSES, type IssueStatus } from "@legion/contracts/dispatch-tools";
import type { IssuePriority, IssueSummary, UpdateIssueInput } from "../../api/types";

/** The Go server's `model.IssueStatuses`, in lifecycle order: the board's columns. */
export const issueStatuses = ISSUE_STATUSES;
export type { IssueStatus };
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

export function isIssueStatus(value: string): value is IssueStatus {
  return (issueStatuses as readonly string[]).includes(value);
}

export function statusLabel(status: IssueStatus): string {
  return statusLabels[status];
}

/** The lifecycle label for a known status; any other string (a filter value the server no
 *  longer knows, a free-text status) is shown as it is. */
export function statusText(status: string): string {
  return isIssueStatus(status) ? statusLabel(status) : status;
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
  if (!isIssueStatus(status)) {
    return undefined;
  }
  return {
    status,
    insertionIndex: issues.filter((issue) => issue.status === status && issue.key !== activeKey)
      .length,
  };
}

/**
 * Places `key` at `insertionIndex` of the `targetStatus` column: the optimistic list to show
 * while the server answers, and the PATCH body naming the visible neighbours (`status` only
 * when the column changes). Undefined when the card is unknown or would not move.
 *
 * Under an active filter `insertionIndex` counts only the cards `isVisible` admits - the ones
 * the board renders - and the PATCH names visible neighbours, so hidden cards may interleave
 * once the filter lifts (the spec's rank rule). The optimistic list still keeps every hidden
 * issue: the card lands directly before the visible card now below it (after the one above it
 * when it becomes the column's last visible card), and hidden cards hold their positions.
 */
export function moveIssue(
  issues: readonly IssueSummary[],
  key: string,
  targetStatus: IssueStatus,
  insertionIndex: number,
  isVisible: (issue: IssueSummary) => boolean = () => true
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
  const visible = target.issues.filter(isVisible);
  const index = Math.min(Math.max(insertionIndex, 0), visible.length);
  const rank = rankInputForInsertion(visible, index);
  const below = visible[index];
  const above = visible[index - 1];
  const spliceAt =
    below !== undefined
      ? target.issues.findIndex((issue) => issue.key === below.key)
      : above !== undefined
        ? target.issues.findIndex((issue) => issue.key === above.key) + 1
        : target.issues.length;
  target.issues.splice(spliceAt, 0, { ...active, status: targetStatus });
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

/** A swimlane: one of the four priorities, or `null` for an issue with none set. */
export type PriorityLane = IssuePriority | null;

/** Lane order: P0 through P3, then issues with no priority - the five bands Sami asked for. */
export const priorityLanes: readonly PriorityLane[] = [0, 1, 2, 3, null];

export function laneLabel(lane: PriorityLane): string {
  return lane === null ? "No priority" : `P${lane}`;
}

/** A lane's value as a DOM attribute and droppable-id fragment: `"0"`-`"3"` or `"none"`. */
export function laneKey(lane: PriorityLane): string {
  return lane === null ? "none" : String(lane);
}

/** The inverse of `laneKey`; `undefined` for anything else. */
export function laneFromKey(value: string): PriorityLane | undefined {
  if (value === "none") {
    return null;
  }
  const parsed = Number(value);
  return (priorityLanes as readonly (IssuePriority | null)[]).includes(parsed as IssuePriority)
    ? (parsed as IssuePriority)
    : undefined;
}

export interface BoardLane {
  readonly lane: PriorityLane;
  readonly columns: BoardColumn[];
}

/**
 * Splits the project list into swimlanes by priority - rows across the nine status columns, one
 * band per priority (P0-P3, then no priority) - each lane itself split into the nine lifecycle
 * columns exactly as `groupIssuesByStatus` does. Rank still orders cards within a lane+column
 * cell, as it orders them within a plain column.
 */
export function groupIssuesByLane(issues: readonly IssueSummary[]): BoardLane[] {
  return priorityLanes.map((lane) => ({
    lane,
    columns: groupIssuesByStatus(issues.filter((issue) => issue.priority === lane)),
  }));
}

/** The droppable id for one lane+status cell: a card dropped here lands in `status` and takes
 *  on `lane`'s priority. */
export function laneColumnId(status: IssueStatus, lane: PriorityLane): string {
  return `lane:${laneKey(lane)}|status:${status}`;
}

function parseLaneColumnId(id: string): { status: IssueStatus; lane: PriorityLane } | undefined {
  const match = /^lane:([^|]+)\|status:(.+)$/.exec(id);
  if (match === null) {
    return undefined;
  }
  const [, lanePart, statusPart] = match;
  const lane = lanePart === undefined ? undefined : laneFromKey(lanePart);
  if (lane === undefined || statusPart === undefined || !isIssueStatus(statusPart)) {
    return undefined;
  }
  return { status: statusPart, lane };
}

export interface LaneDropTarget {
  readonly status: IssueStatus;
  readonly lane: PriorityLane;
  readonly insertionIndex: number;
}

/**
 * The lane-aware `dropTarget`: `overId` is a card key or a `lane:<l>|status:<s>` cell id (full
 * or collapsed). A drop onto a card takes that card's own status and priority as the target
 * cell; a drop onto a cell takes the cell's. Dragging a card into a different lane reassigns its
 * priority - the same gesture that reassigns status when a card crosses a column, now
 * reassigning the lane's own dimension.
 */
export function laneDropTarget(
  issues: readonly IssueSummary[],
  activeKey: string,
  overId: string
): LaneDropTarget | undefined {
  if (activeKey === overId) {
    return undefined;
  }
  if (!issues.some((issue) => issue.key === activeKey)) {
    return undefined;
  }
  const overIssue = issues.find((issue) => issue.key === overId);
  if (overIssue !== undefined) {
    const status = overIssue.status as IssueStatus;
    const lane = overIssue.priority;
    const cell = issues.filter((issue) => issue.status === status && issue.priority === lane);
    return { status, lane, insertionIndex: cell.findIndex((issue) => issue.key === overId) };
  }
  const parsed = parseLaneColumnId(overId);
  if (parsed === undefined) {
    return undefined;
  }
  return {
    ...parsed,
    insertionIndex: issues.filter(
      (issue) =>
        issue.status === parsed.status && issue.priority === parsed.lane && issue.key !== activeKey
    ).length,
  };
}

/**
 * `moveIssue`'s lane-aware twin: places `key` at `insertionIndex` of the `targetLane`+
 * `targetStatus` cell, naming the cell's visible neighbours for `rank` and setting `status`
 * and/or `priority` in the same PATCH when either changes.
 */
export function moveIssueToLane(
  issues: readonly IssueSummary[],
  key: string,
  targetStatus: IssueStatus,
  targetLane: PriorityLane,
  insertionIndex: number,
  isVisible: (issue: IssueSummary) => boolean = () => true
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
  const cell = target.issues.filter((issue) => issue.priority === targetLane && isVisible(issue));
  const index = Math.min(Math.max(insertionIndex, 0), cell.length);
  const rank = rankInputForInsertion(cell, index);
  const below = cell[index];
  const above = cell[index - 1];
  const spliceAt =
    below !== undefined
      ? target.issues.findIndex((issue) => issue.key === below.key)
      : above !== undefined
        ? target.issues.findIndex((issue) => issue.key === above.key) + 1
        : target.issues.length;
  target.issues.splice(spliceAt, 0, { ...active, status: targetStatus, priority: targetLane });
  const moved = columns.flatMap((column) => column.issues);
  if (
    active.status === targetStatus &&
    active.priority === targetLane &&
    moved.every((issue, position) => issue.key === issues[position]?.key)
  ) {
    return undefined;
  }
  return {
    issues: moved,
    input: {
      ...(active.status === targetStatus ? {} : { status: targetStatus }),
      ...(active.priority === targetLane ? {} : { priority: targetLane }),
      rank,
    },
  };
}
