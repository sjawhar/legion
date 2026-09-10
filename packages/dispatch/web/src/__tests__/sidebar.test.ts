import { expect, test } from "bun:test";
import type { IssueSummary, UserState } from "../api/types";
import { arrangeIssues, selectSidebarView } from "../features/sidebar/Sidebar";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key: "CORE-1",
    open_asks: 0,
    parent: null,
    status: "todo",
    title: "Core work",
    updated_at: "2026-09-09T00:00:00Z",
    ...overrides,
  };
}

test("sidebar ranks pinned, needs you, unread, and remaining issues by activity", () => {
  const issues = [
    issue({ key: "CORE-1", title: "Older", updated_at: "2026-09-09T01:00:00Z" }),
    issue({ key: "CORE-2", title: "Needs you", open_asks: 2, updated_at: "2026-09-09T02:00:00Z" }),
    issue({ key: "CORE-3", title: "Unread", updated_at: "2026-09-09T03:00:00Z" }),
    issue({ key: "CORE-4", title: "Pinned", updated_at: "2026-09-09T00:00:00Z" }),
    issue({ key: "CORE-5", title: "Newer", updated_at: "2026-09-09T04:00:00Z" }),
  ];
  const state: UserState = {
    "CORE-3": { dismissed: [], last_read_seq: 1, pinned: false },
    "CORE-4": { dismissed: [], last_read_seq: 8, pinned: true },
  };

  expect(arrangeIssues(issues, state, { "CORE-3": 2 })).toEqual([
    { count: 1, items: ["CORE-4"], label: "Pinned" },
    { count: 2, items: ["CORE-2"], label: "Needs you" },
    { count: 1, items: ["CORE-3"], label: "Unread" },
    { count: 2, items: ["CORE-5", "CORE-1"], label: "Everything else" },
  ]);
});

test("sidebar nests children under a listed parent", () => {
  const issues = [
    issue({ key: "CORE-1", title: "Parent", updated_at: "2026-09-09T01:00:00Z" }),
    issue({
      key: "CORE-2",
      parent: "CORE-1",
      title: "Child with an ask",
      open_asks: 1,
      updated_at: "2026-09-09T02:00:00Z",
    }),
  ];

  expect(arrangeIssues(issues, {}, {})).toEqual([
    {
      count: 1,
      items: [
        {
          children: ["CORE-2"],
          key: "CORE-1",
        },
      ],
      label: "Needs you",
    },
  ]);
});

test("sidebar hides groups with no issues instead of rendering a dangling header", () => {
  const issues = [issue({ key: "CORE-1", title: "Only issue" })];

  expect(arrangeIssues(issues, {}, {}).map((group) => group.label)).toEqual(["Everything else"]);
});

test("sidebar keeps its active issue snapshot while new detail data is pending", () => {
  const frozen = [{ count: 1, items: ["CORE-1"], label: "Pinned" as const }];
  const latest = [{ count: 0, items: ["CORE-2"], label: "Everything else" as const }];

  const view = selectSidebarView("CORE-1", { groups: frozen, issueKey: "CORE-1" }, latest, true);

  expect(view.displayed).toBe(frozen);
  const afterLeaving = selectSidebarView(
    undefined,
    { groups: frozen, issueKey: "CORE-1" },
    latest,
    true
  );
  expect(afterLeaving.frozen).toBeUndefined();
});
