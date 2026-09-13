import { expect, test } from "bun:test";

import type { IssueSummary } from "../../api/types";
import { groupIssuesByStatus, rankInputForInsertion } from "./board-model";

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

test("groups issues by lifecycle with each board column in rank order", () => {
  const columns = groupIssuesByStatus([
    issue({ key: "CORE-3", status: "todo", rank: "k" }),
    issue({ key: "CORE-1", status: "triage", rank: "U" }),
    issue({ key: "CORE-2", status: "todo", rank: "f" }),
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

test("orders a board column by rank even when the server list is priority ordered", () => {
  const columns = groupIssuesByStatus([
    issue({ key: "CORE-1", priority: 0, rank: "U" }),
    issue({ key: "CORE-2", priority: 2, rank: "F" }),
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
});
