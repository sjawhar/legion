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

test("board keeps lifecycle headers structurally consistent and describes daemon locks", async () => {
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
    const headerChildCount = headers[0]?.firstElementChild?.children.length;
    expect(headerChildCount).toBe(3);
    expect(
      headers.every((header) => header.firstElementChild?.children.length === headerChildCount)
    ).toBe(true);

    const daemonColumn = within(board).getByRole("region", { name: "In progress" });
    const lock = within(daemonColumn).getByRole("img", { name: "Daemon controlled" });
    expect(lock.getAttribute("aria-description")).toBe(
      "Only the Legion daemon can move issues to In progress."
    );
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});
