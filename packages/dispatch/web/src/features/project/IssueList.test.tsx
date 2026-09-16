import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary, UserState } from "../../api/types";
import { IssueList } from "./IssueList";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 0,
    parent: null,
    assignee: null,
    status: "todo",
    priority: null,
    rank: "U",
    title: "Core work",
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

function renderList(issues: IssueSummary[], state: UserState = {}, search = "") {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(issues);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(state);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[`/projects/CORE${search}`]}>
      <QueryClientProvider client={queryClient}>
        <IssueList project="CORE" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getMyState, listIssues, view };
}

test("groups issues by status in board order with done collapsed and empty groups hidden", async () => {
  const { getMyState, listIssues, view } = renderList([
    issue({ key: "CORE-1", status: "testing" }),
    issue({ key: "CORE-2", status: "todo" }),
    issue({ key: "CORE-3", status: "done" }),
  ]);

  try {
    await screen.findByRole("link", { name: /CORE-1.*Core work/ });
    expect(
      screen
        .getAllByRole("group")
        .filter((group) => group.tagName === "DETAILS")
        .map((group) => group.getAttribute("aria-label"))
    ).toEqual(["Todo (1)", "Testing (1)", "Done (1)"]);
    const done = screen.getByText("Done (1)").closest("details");
    expect(done).toBeInstanceOf(HTMLDetailsElement);
    expect((done as HTMLDetailsElement).open).toBe(false);
    expect(listIssues).toHaveBeenCalledWith({ project: "CORE" });
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("repeated ?status= values narrow the list to those lifecycle groups", async () => {
  const { getMyState, listIssues, view } = renderList(
    [
      issue({ key: "CORE-1", status: "testing" }),
      issue({ key: "CORE-2", status: "todo" }),
      issue({ key: "CORE-3", status: "done" }),
    ],
    {},
    "?status=todo&status=testing"
  );

  try {
    await screen.findByRole("link", { name: /CORE-2.*Core work/ });
    expect(
      screen
        .getAllByRole("group")
        .filter((group) => group.tagName === "DETAILS")
        .map((group) => group.getAttribute("aria-label"))
    ).toEqual(["Todo (1)", "Testing (1)"]);
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("a child row shows its parent chip", async () => {
  const { getMyState, listIssues, view } = renderList([
    issue({ key: "CORE-2", parent: "CORE-1", title: "Child" }),
  ]);

  try {
    await screen.findByRole("link", { name: /CORE-2.*Child/ });
    expect(screen.getByRole("link", { name: "CORE-1" }).getAttribute("href")).toBe(
      "/issues/CORE-1"
    );
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});
