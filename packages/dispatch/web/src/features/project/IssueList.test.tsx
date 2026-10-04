import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary, UserState } from "../../api/types";
import { IssueList } from "./IssueList";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    route: null,
    route_status: null,
    route_holder: null,
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 0,
    progress: { tasks: null, children: null },
    parent: null,
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
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

test("marks only the rows whose route reaches nobody", async () => {
  const { getMyState, listIssues, view } = renderList([
    issue({ key: "CORE-1", title: "Unheld role", route: "role:sre", route_status: "no_holder" }),
    issue({
      key: "CORE-2",
      title: "Gone session",
      route: "session:ses-gone",
      route_status: "no_holder",
    }),
    issue({
      key: "CORE-3",
      title: "Held role",
      route: "role:platform-po",
      route_status: "live",
      route_holder: "ses-po",
    }),
    issue({ key: "CORE-4", title: "Unjudged route", route: "role:sre", route_status: "unknown" }),
    issue({ key: "CORE-5", title: "Unrouted" }),
  ]);

  try {
    await screen.findByRole("link", { name: /CORE-1.*Unheld role/ });
    const marked = screen
      .getAllByTestId("issue-route-unreachable")
      .map((marker) => [marker.closest("li")?.getAttribute("aria-label"), marker.textContent]);
    expect(marked).toEqual([
      ["CORE-1 Unheld role", expect.stringContaining("Nobody holds it right now")],
      ["CORE-2 Gone session", expect.stringContaining("Not running right now")],
    ]);
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("a row shows each progress count the server counted and nothing for a null", async () => {
  const { getMyState, listIssues, view } = renderList([
    issue({
      key: "CORE-1",
      title: "Both counted",
      progress: { tasks: { done: 3, total: 7 }, children: { done: 2, total: 5 } },
    }),
    issue({
      key: "CORE-2",
      title: "Tasks only",
      progress: { tasks: { done: 0, total: 2 }, children: null },
    }),
    issue({ key: "CORE-3", title: "Neither" }),
  ]);

  try {
    await screen.findByRole("link", { name: /CORE-1.*Both counted/ });
    const chips = [
      ...screen.getAllByTestId("issue-progress-tasks"),
      ...screen.getAllByTestId("issue-progress-children"),
    ].map((chip) => [chip.closest("li")?.getAttribute("aria-label"), chip.textContent]);
    expect(chips).toEqual([
      ["CORE-1 Both counted", "3/7 tasks"],
      ["CORE-2 Tasks only", "0/2 tasks"],
      ["CORE-1 Both counted", "2/5 children"],
    ]);
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});
