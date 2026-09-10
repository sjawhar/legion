import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type { IssueSummary, UserState } from "../api/types";
import { arrangeIssues, Sidebar } from "../features/sidebar/Sidebar";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key: "CORE-1",
    open_asks: 0,
    parent: null,
    status: "todo",
    last_seq: 0,
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

test("sidebar renders listed issues without fetching individual issue details", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(undefined as never);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([issue()]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/"] },
      createElement(QueryClientProvider, { client: queryClient }, createElement(Sidebar))
    )
  );

  try {
    await screen.findByRole("link", { name: /CORE-1.*Core work/ });
    expect(getIssue).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});
