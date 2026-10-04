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
 * Resolves a dnd-kit drop against a board's cells, shared by `dropTarget` (cells are lifecycle
 * statuses) and `laneDropTarget` (cells are lane+status pairs). `cellOf` names the cell an issue
 * currently occupies, `cellId` turns a cell into the string two cells compare equal by, and
 * `parseCellId` recovers a cell from a dropped-on column or rail's own id. A drop onto a card
 * lands at that card's position in its own cell (dnd-kit's `arrayMove`: one step down lands
 * below the card it passed, one step up lands above it); a drop onto a cell's column or rail
 * appends. Undefined when the drop is not a move.
 */
function resolveDropTarget<Cell>(
  issues: readonly IssueSummary[],
  activeKey: string,
  overId: string,
  cellOf: (issue: IssueSummary) => Cell,
  cellId: (cell: Cell) => string,
  parseCellId: (id: string) => Cell | undefined
): { cell: Cell; insertionIndex: number } | undefined {
  if (activeKey === overId) {
    return undefined;
  }
  if (!issues.some((issue) => issue.key === activeKey)) {
    return undefined;
  }
  const overIssue = issues.find((issue) => issue.key === overId);
  if (overIssue !== undefined) {
    const cell = cellOf(overIssue);
    const id = cellId(cell);
    const cellIssues = issues.filter((issue) => cellId(cellOf(issue)) === id);
    return { cell, insertionIndex: cellIssues.findIndex((issue) => issue.key === overId) };
  }
  const cell = parseCellId(overId);
  if (cell === undefined) {
    return undefined;
  }
  const id = cellId(cell);
  return {
    cell,
    insertionIndex: issues.filter(
      (issue) => cellId(cellOf(issue)) === id && issue.key !== activeKey
    ).length,
  };
}

/**
 * Translates a dnd-kit drop - `overId` is another card's key or a column's `status:<s>` id -
 * into the column and position `moveCard` takes.
 */
export function dropTarget(
  issues: readonly IssueSummary[],
  activeKey: string,
  overId: string
): DropTarget | undefined {
  const resolved = resolveDropTarget<IssueStatus>(
    issues,
    activeKey,
    overId,
    (issue) => issue.status as IssueStatus,
    (status) => status,
    (id) => {
      const status = id.replace(/^status:/, "");
      return isIssueStatus(status) ? status : undefined;
    }
  );
  return resolved === undefined
    ? undefined
    : { status: resolved.cell, insertionIndex: resolved.insertionIndex };
}

/**
 * Shared splice-and-rank logic for `moveIssue` and `moveIssueToLane`: groups `issues` by status,
 * places `key` at `insertionIndex` of the cell `cellPredicate` admits within the `targetStatus`
 * column, and returns the optimistic list plus the neighbours for `rank`. `isNoOp` is the
 * caller's own answer to "would the active card stay exactly where it already is" - `moveIssue`
 * asks about status alone, `moveIssueToLane` about status and priority together - and this
 * returns `undefined` instead of a result when it is.
 */
function placeInColumn(
  issues: readonly IssueSummary[],
  key: string,
  targetStatus: IssueStatus,
  insertionIndex: number,
  cellPredicate: (issue: IssueSummary) => boolean,
  applyTarget: (issue: IssueSummary) => IssueSummary,
  isNoOp: (active: IssueSummary) => boolean,
  isVisible: (issue: IssueSummary) => boolean
): { issues: IssueSummary[]; rank: { before?: string; after?: string } } | undefined {
  const active = issues.find((issue) => issue.key === key);
  if (active === undefined) {
    return undefined;
  }
  const columns = groupIssuesByStatus(issues.filter((issue) => issue.key !== key));
  const target = columns.find((column) => column.status === targetStatus);
  if (target === undefined) {
    return undefined;
  }
  const cell = target.issues.filter((issue) => cellPredicate(issue) && isVisible(issue));
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
  target.issues.splice(spliceAt, 0, applyTarget(active));
  const moved = columns.flatMap((column) => column.issues);
  if (isNoOp(active) && moved.every((issue, position) => issue.key === issues[position]?.key)) {
    return undefined;
  }
  return { issues: moved, rank };
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
  const placed = placeInColumn(
    issues,
    key,
    targetStatus,
    insertionIndex,
    () => true,
    (issue) => ({ ...issue, status: targetStatus }),
    (candidate) => candidate.status === targetStatus,
    isVisible
  );
  if (placed === undefined) {
    return undefined;
  }
  return {
    issues: placed.issues,
    input: {
      ...(active.status === targetStatus ? {} : { status: targetStatus }),
      rank: placed.rank,
    },
  };
}

/** A swimlane: one of the four priorities, or `null` for an issue with none set. */
export type PriorityLane = IssuePriority | null;

/** Lane order: P0 through P3, then issues with no priority. */
export const priorityLanes: readonly PriorityLane[] = [0, 1, 2, 3, null];

export function laneLabel(lane: PriorityLane): string {
  return lane === null ? "No priority" : `P${lane}`;
}

/** A lane's value as a DOM attribute and droppable-id fragment: `"0"`-`"3"` or `"none"`. */
export function laneKey(lane: PriorityLane): string {
  return lane === null ? "none" : String(lane);
}

/** The inverse of `laneKey`; `undefined` for anything else, including an empty string (`Number`
 *  reads `""` as `0`, which would otherwise be mistaken for P0). */
export function laneFromKey(value: string): PriorityLane | undefined {
  if (value === "none") {
    return null;
  }
  return /^[0-3]$/.test(value) ? (Number(value) as IssuePriority) : undefined;
}

export interface BoardLane {
  readonly lane: PriorityLane;
  readonly columns: BoardColumn[];
}

/**
 * Splits the project list into swimlanes by priority - rows across the nine status columns, one
 * band per priority (P0-P3, then no priority) - each lane itself split into the nine lifecycle
 * columns. Rank still orders cards within a lane+column cell, as it orders them within a plain
 * column. One pass over `issues` buckets every lane+status pair at once, rather than filtering
 * the whole list once per lane.
 */
export function groupIssuesByLane(issues: readonly IssueSummary[]): BoardLane[] {
  const cells: Record<string, IssueSummary[]> = {};
  for (const issue of issues) {
    const key = `${laneKey(issue.priority)}|${issue.status}`;
    const bucket = cells[key];
    if (bucket === undefined) {
      cells[key] = [issue];
    } else {
      bucket.push(issue);
    }
  }
  return priorityLanes.map((lane) => ({
    lane,
    columns: issueStatuses.map((status) => ({
      status,
      issues: cells[`${laneKey(lane)}|${status}`] ?? [],
    })),
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
  const resolved = resolveDropTarget<{ status: IssueStatus; lane: PriorityLane }>(
    issues,
    activeKey,
    overId,
    (issue) => ({ status: issue.status as IssueStatus, lane: issue.priority }),
    (cell) => laneColumnId(cell.status, cell.lane),
    parseLaneColumnId
  );
  return resolved === undefined
    ? undefined
    : {
        status: resolved.cell.status,
        lane: resolved.cell.lane,
        insertionIndex: resolved.insertionIndex,
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
  const placed = placeInColumn(
    issues,
    key,
    targetStatus,
    insertionIndex,
    (issue) => issue.priority === targetLane,
    (issue) => ({ ...issue, status: targetStatus, priority: targetLane }),
    (candidate) => candidate.status === targetStatus && candidate.priority === targetLane,
    isVisible
  );
  if (placed === undefined) {
    return undefined;
  }
  return {
    issues: placed.issues,
    input: {
      ...(active.status === targetStatus ? {} : { status: targetStatus }),
      ...(active.priority === targetLane ? {} : { priority: targetLane }),
      rank: placed.rank,
    },
  };
}
