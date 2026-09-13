import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { issueStatuses } from "./board-model";
import { IssueBoard } from "./IssueBoard";

const issues: IssueSummary[] = [
  {
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    open_asks: 0,
    parent: null,
    rank: "U",
    priority: null,
    status: "triage",
    title: "Plan the work",
    updated_at: "2026-09-13T00:00:00Z",
  },
];

test("board renders every lifecycle column without daemon-only status controls", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(issues);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <IssueBoard project="CORE" />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const headers = within(board).getAllByTestId("board-column-header");
    expect(headers).toHaveLength(issueStatuses.length);
    expect(headers.every((header) => header.classList.contains("min-h-[52px]"))).toBe(true);
    expect(within(board).queryByRole("img", { name: "Daemon controlled" })).toBeNull();
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});

test("board shows a shared empty state when the project has no issues", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <IssueBoard project="CORE" />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    expect(
      within(board).getByRole("region", { name: "Empty project board" }).textContent
    ).toContain("No issues in this project.");
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});
