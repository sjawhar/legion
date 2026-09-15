import { type BoardColumn, type IssueStatus, issueStatuses, statusLabel } from "./board-model";

/** Where keyboard focus sits on the board: `index: null` is the column or rail itself. */
export interface BoardFocus {
  readonly status: IssueStatus;
  readonly index: number | null;
}

export type RoveKey = "j" | "k" | "h" | "l";
export type MoveDirection = "down" | "up" | "prev" | "next";

/** The card at `index` of `status`, clamped into the column; the column itself when it is empty
 *  or collapsed (a rail renders no cards). */
function landIn(
  columns: readonly BoardColumn[],
  isCollapsed: (status: IssueStatus) => boolean,
  status: IssueStatus,
  index: number | null
): BoardFocus {
  const count = columns.find((column) => column.status === status)?.issues.length ?? 0;
  if (count === 0 || isCollapsed(status)) {
    return { status, index: null };
  }
  return { status, index: Math.min(Math.max(index ?? 0, 0), count - 1) };
}

/**
 * The next focus target for a roving key. `j`/`k` clamp within the column; `h`/`l` step to the
 * adjacent lifecycle status (rails included) keeping the index, clamped into the shorter column,
 * and land on the column or rail itself when it holds no card to focus. At the board's ends the
 * focus stays. With nothing focused, `j`/`l` pick the first card of the first non-empty
 * non-collapsed column and `k`/`h` the last card of the last such column; with no card to pick
 * they land on the first or last column itself.
 */
export function focusTarget(
  columns: readonly BoardColumn[],
  isCollapsed: (status: IssueStatus) => boolean,
  current: BoardFocus | null,
  key: RoveKey
): BoardFocus {
  const first = key === "j" || key === "l";
  if (current === null) {
    const ordered = first ? columns : [...columns].reverse();
    const column = ordered.find((entry) => entry.issues.length > 0 && !isCollapsed(entry.status));
    if (column !== undefined) {
      return { status: column.status, index: first ? 0 : column.issues.length - 1 };
    }
    const edge = ordered[0];
    return edge === undefined
      ? { status: "triage", index: null }
      : { status: edge.status, index: null };
  }
  const position = issueStatuses.indexOf(current.status);
  switch (key) {
    case "j":
    case "k": {
      if (current.index === null) {
        return current;
      }
      return landIn(columns, isCollapsed, current.status, current.index + (key === "j" ? 1 : -1));
    }
    case "h":
    case "l": {
      const adjacent = issueStatuses[position + (key === "l" ? 1 : -1)];
      return adjacent === undefined
        ? current
        : landIn(columns, isCollapsed, adjacent, current.index);
    }
  }
}

/**
 * Where a keyboard move sends `key`, in `moveIssue`'s convention: the insertion index counts the
 * column without the moving card. `down`/`up` shift one place and stop at the column's ends;
 * `prev`/`next` land at the TOP of the adjacent status (Linear's rule for keyboard status moves;
 * into Done closes and out of Done reopens on the server, as a drag would) and stop at Triage and
 * Done. Undefined when there is nothing to do - no request, no announcement.
 */
export function keyboardMove(
  columns: readonly BoardColumn[],
  key: string,
  direction: MoveDirection
): { status: IssueStatus; insertionIndex: number } | undefined {
  const column = columns.find((entry) => entry.issues.some((issue) => issue.key === key));
  if (column === undefined) {
    return undefined;
  }
  const index = column.issues.findIndex((issue) => issue.key === key);
  switch (direction) {
    case "down":
      return index === column.issues.length - 1
        ? undefined
        : { status: column.status, insertionIndex: index + 1 };
    case "up":
      return index === 0 ? undefined : { status: column.status, insertionIndex: index - 1 };
    case "prev":
    case "next": {
      const status =
        issueStatuses[issueStatuses.indexOf(column.status) + (direction === "next" ? 1 : -1)];
      return status === undefined ? undefined : { status, insertionIndex: 0 };
    }
  }
}

/** What the live region reads after a move: `"CORE-12 → Todo, position 2 of 5"`, or
 *  `"CORE-12 → Done, closed"` since a card moved into Done is closed rather than ranked. */
export function announceMove(
  key: string,
  status: IssueStatus,
  position: number,
  count: number
): string {
  return status === "done"
    ? `${key} → ${statusLabel(status)}, closed`
    : `${key} → ${statusLabel(status)}, position ${position} of ${count}`;
}
