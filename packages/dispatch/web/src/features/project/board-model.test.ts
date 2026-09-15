import { expect, test } from "bun:test";

import type { IssueSummary } from "../../api/types";
import { dropTarget, groupIssuesByStatus, moveIssue, rankInputForInsertion } from "./board-model";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key: "CORE-1",
    title: "Core work",
    status: "todo",
    priority: null,
    rank: "U",
    labels: [],
    parent: null,
    updated_at: "2026-09-12T00:00:00Z",
    last_seq: 1,
    open_asks: 0,
    ...overrides,
  };
}

test("groups issues by lifecycle, keeping each column in the list's (rank) order", () => {
  const columns = groupIssuesByStatus([
    issue({ key: "CORE-1", status: "triage", rank: "U" }),
    issue({ key: "CORE-2", status: "todo", rank: "f" }),
    issue({ key: "CORE-3", status: "todo", rank: "k" }),
  ]);

  expect(columns.map((column) => column.status)).toEqual([
    "triage",
    "icebox",
    "backlog",
    "todo",
    "in_progress",
    "testing",
    "needs_review",
    "retro",
    "done",
  ]);
  expect(
    columns.find((column) => column.status === "todo")?.issues.map((item) => item.key)
  ).toEqual(["CORE-2", "CORE-3"]);
});

test("keeps an optimistic reorder in place instead of re-sorting the column by stale rank", () => {
  // After a drag the moved card still carries its old rank until the server answers; the
  // column must show it where it was dropped, not snap it back until the refetch.
  const columns = groupIssuesByStatus([
    issue({ key: "CORE-2", rank: "k" }),
    issue({ key: "CORE-1", rank: "U" }),
  ]);

  expect(
    columns.find((column) => column.status === "todo")?.issues.map((item) => item.key)
  ).toEqual(["CORE-2", "CORE-1"]);
});

test("derives before and after keys from the final insertion position", () => {
  const issues = [
    issue({ key: "CORE-1", rank: "F" }),
    issue({ key: "CORE-2", rank: "U" }),
    issue({ key: "CORE-3", rank: "k" }),
  ];

  expect(rankInputForInsertion(issues, 0)).toEqual({ before: "CORE-1" });
  expect(rankInputForInsertion(issues, 1)).toEqual({ after: "CORE-1", before: "CORE-2" });
  expect(rankInputForInsertion(issues, 3)).toEqual({ after: "CORE-3" });
  expect(rankInputForInsertion([], 0)).toEqual({});
});

const board = [
  issue({ key: "CORE-1", status: "todo", rank: "F" }),
  issue({ key: "CORE-2", status: "todo", rank: "U" }),
  issue({ key: "CORE-3", status: "todo", rank: "k" }),
  issue({ key: "CORE-4", status: "in_progress", rank: "p" }),
  issue({ key: "CORE-5", status: "done", rank: "z" }),
];

test("a drop onto a card in the same column lands on that card's position in either direction", () => {
  // Down one card: CORE-1 over CORE-2 ends up below CORE-2 (dnd-kit's arrayMove semantics).
  expect(dropTarget(board, "CORE-1", "CORE-2")).toEqual({ status: "todo", insertionIndex: 1 });
  // Down two cards lands at the bottom.
  expect(dropTarget(board, "CORE-1", "CORE-3")).toEqual({ status: "todo", insertionIndex: 2 });
  // Up: CORE-3 over CORE-1 lands on top.
  expect(dropTarget(board, "CORE-3", "CORE-1")).toEqual({ status: "todo", insertionIndex: 0 });
});

test("a drop onto another column's card lands above it; onto the column itself appends", () => {
  expect(dropTarget(board, "CORE-1", "CORE-4")).toEqual({
    status: "in_progress",
    insertionIndex: 0,
  });
  expect(dropTarget(board, "CORE-1", "status:in_progress")).toEqual({
    status: "in_progress",
    insertionIndex: 1,
  });
  // A collapsed Done rail is the `status:done` droppable: the card is appended after the
  // column's existing cards.
  expect(dropTarget(board, "CORE-4", "status:done")).toEqual({ status: "done", insertionIndex: 1 });
  expect(dropTarget(board, "CORE-4", "status:icebox")).toEqual({
    status: "icebox",
    insertionIndex: 0,
  });
});

test("a drop onto itself, an unknown target or an unknown card is not a move", () => {
  expect(dropTarget(board, "CORE-1", "CORE-1")).toBeUndefined();
  expect(dropTarget(board, "CORE-1", "status:nowhere")).toBeUndefined();
  expect(dropTarget(board, "CORE-9", "CORE-1")).toBeUndefined();
});

test("moveIssue places the card optimistically and names the visible neighbours", () => {
  const down = moveIssue(board, "CORE-1", "todo", 2);
  expect(down?.issues.map((item) => item.key)).toEqual([
    "CORE-2",
    "CORE-3",
    "CORE-1",
    "CORE-4",
    "CORE-5",
  ]);
  expect(down?.input).toEqual({ rank: { after: "CORE-3" } });

  const across = moveIssue(board, "CORE-3", "in_progress", 0);
  expect(across?.issues.map((item) => `${item.key}:${item.status}`)).toEqual([
    "CORE-1:todo",
    "CORE-2:todo",
    "CORE-3:in_progress",
    "CORE-4:in_progress",
    "CORE-5:done",
  ]);
  expect(across?.input).toEqual({ status: "in_progress", rank: { before: "CORE-4" } });

  // Into an empty column: no neighbours, the server appends at the project's last rank.
  expect(moveIssue(board, "CORE-5", "icebox", 0)?.input).toEqual({
    status: "icebox",
    rank: {},
  });
});

test("moveIssue is a no-op when the card would stay where it is", () => {
  expect(moveIssue(board, "CORE-1", "todo", 0)).toBeUndefined();
  expect(moveIssue(board, "CORE-3", "todo", 2)).toBeUndefined();
  expect(moveIssue(board, "CORE-9", "todo", 0)).toBeUndefined();
});
