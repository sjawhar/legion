import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
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
    status: "todo",
    title: "Core work",
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

function renderList(issues: IssueSummary[], state: UserState = {}) {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(issues);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(state);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
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
    expect(screen.getAllByRole("group").map((group) => group.getAttribute("aria-label"))).toEqual([
      "todo (1)",
      "testing (1)",
      "done (1)",
    ]);
    const done = screen.getByText("done (1)").closest("details");
    expect(done).toBeInstanceOf(HTMLDetailsElement);
    expect((done as HTMLDetailsElement).open).toBe(false);
    expect(listIssues).toHaveBeenCalledWith({ project: "CORE" });
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("Needs you keeps only issues with open asks", async () => {
  const { getMyState, listIssues, view } = renderList([
    issue({ key: "CORE-1", open_asks: 1, title: "Answer me" }),
    issue({ key: "CORE-2", title: "Quiet" }),
  ]);

  try {
    await screen.findByText("Answer me");
    fireEvent.click(screen.getByRole("button", { name: "Needs you" }));
    expect(screen.getByText("Answer me")).toBeTruthy();
    expect(screen.queryByText("Quiet")).toBeNull();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("Unread keeps only issues with events past last_read_seq", async () => {
  const { getMyState, listIssues, view } = renderList(
    [
      issue({ key: "CORE-1", last_seq: 4, title: "Unread" }),
      issue({ key: "CORE-2", last_seq: 3, title: "Read" }),
    ],
    {
      "CORE-1": { dismissed: [], last_read_seq: 3, pinned: false },
      "CORE-2": { dismissed: [], last_read_seq: 3, pinned: false },
    }
  );

  try {
    await screen.findByRole("link", { name: /CORE-1.*Unread/ });
    fireEvent.click(screen.getByRole("button", { name: "Unread" }));
    expect(screen.getByRole("link", { name: /CORE-1.*Unread/ })).toBeTruthy();
    expect(screen.queryByText("Read")).toBeNull();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("label filter and title search narrow the list", async () => {
  const { getMyState, listIssues, view } = renderList([
    issue({ key: "CORE-1", labels: ["frontend"], title: "Design project navigation" }),
    issue({ key: "CORE-2", labels: ["backend"], title: "Add project endpoint" }),
  ]);

  try {
    await screen.findByText("Design project navigation");
    fireEvent.change(screen.getByRole("combobox", { name: "Label" }), {
      target: { value: "frontend" },
    });
    expect(screen.queryByText("Add project endpoint")).toBeNull();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), {
      target: { value: "navigation" },
    });
    expect(screen.getByText("Design project navigation")).toBeTruthy();
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
