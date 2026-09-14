import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Issue, IssueSummary, UserState } from "../../api/types";
import { issueStatuses } from "./board-model";
import { staleBoardMessage, useBoardMoves } from "./board-moves";
import { IssueBoard } from "./IssueBoard";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
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
    ...overrides,
  };
}

const issues: IssueSummary[] = [
  issue(),
  issue({ key: "CORE-2", last_seq: 3, rank: "k", status: "todo", title: "Ship the work" }),
  issue({ key: "CORE-3", rank: "p", status: "done", title: "Shipped" }),
];

function renderBoard(state: UserState = {}, showEdges?: boolean) {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(issues);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(state);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <IssueBoard project="CORE" showEdges={showEdges} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    cleanup() {
      view.unmount();
      listIssues.mockRestore();
      getMyState.mockRestore();
    },
  };
}

test("board collapses Icebox and Done into droppable rails by default", async () => {
  const { cleanup } = renderBoard();
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const headers = within(board).getAllByTestId("board-column-header");
    expect(headers).toHaveLength(issueStatuses.length - 2);
    expect(headers.every((header) => header.classList.contains("min-h-[52px]"))).toBe(true);
    const rails = within(board).getAllByTestId("board-column-rail");
    expect(rails).toHaveLength(2);
    const done = within(board).getByRole("region", { name: "Done (collapsed)" });
    expect(done.textContent).toContain("Done");
    expect(done.textContent).toContain("1");
    expect(within(done).queryByRole("article")).toBeNull();
    expect(within(board).getByRole("region", { name: "Icebox (collapsed)" })).toBeDefined();
  } finally {
    cleanup();
  }
});

test("board shows all nine columns when the edges are expanded", async () => {
  const { cleanup } = renderBoard({}, true);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    expect(within(board).getAllByTestId("board-column-header")).toHaveLength(issueStatuses.length);
    expect(within(board).queryAllByTestId("board-column-rail")).toHaveLength(0);
    expect(
      within(within(board).getByRole("region", { name: "Done" })).getByRole("article", {
        name: "CORE-3 Shipped",
      })
    ).toBeDefined();
  } finally {
    cleanup();
  }
});

test("the whole card is the drag activator: no Reorder button, no status pill, still an article", async () => {
  const { cleanup } = renderBoard();
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const card = within(board).getByRole("article", { name: "CORE-1 Plan the work" });
    expect(card.tagName).toBe("ARTICLE");
    expect(card.getAttribute("tabindex")).toBe("0");
    expect(within(board).queryAllByRole("button", { name: /^Reorder/ })).toHaveLength(0);
    // The column header names the status; the card no longer repeats it.
    expect(within(card).queryByText("Triage")).toBeNull();
    expect(within(card).getByRole("link", { name: /CORE-1/ })).toBeDefined();
    expect(within(card).getByRole("combobox", { name: "Priority of CORE-1" })).toBeDefined();
  } finally {
    cleanup();
  }
});

test("board cards show the unread dot when events are newer than the viewer's last read", async () => {
  const { cleanup } = renderBoard({
    "CORE-1": { dismissed: [], last_read_seq: 1, pinned: false },
    "CORE-2": { dismissed: [], last_read_seq: 2, pinned: false },
  });
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const unread = within(board).getByRole("article", { name: "CORE-2 Ship the work" });
    await waitFor(() => {
      expect(unread.querySelector('[title="Unread"]')).not.toBeNull();
    });
    const read = within(board).getByRole("article", { name: "CORE-1 Plan the work" });
    expect(read.querySelector('[title="Unread"]')).toBeNull();
  } finally {
    cleanup();
  }
});

test("board shows a shared empty state when the project has no issues", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
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
    getMyState.mockRestore();
  }
});

const patched: Issue = {
  closed_at: null,
  created_at: "2026-09-13T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "CORE",
  rank: "U",
  route: null,
  status: "todo",
  title: "Plan the work",
  updated_at: "2026-09-13T00:00:00Z",
};

function renderMoves(queryClient: QueryClient) {
  return renderHook(() => useBoardMoves("CORE"), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    ),
  });
}

test("a failed move rolls the board back, refetches it and tells the truth about a stale rank", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, issues);
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(
    new ApiError(400, { code: "RANK_INPUT", error: "rank neighbors are not ordered" })
  );
  const hook = renderMoves(queryClient);
  try {
    await act(() => hook.result.current.moveCard("CORE-1", "todo", 1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", {
      rank: { after: "CORE-2" },
      status: "todo",
    });
    expect(hook.result.current.error).toBe(staleBoardMessage);
    expect(queryClient.getQueryData<IssueSummary[]>(queryKey)).toEqual(issues);
    expect(queryClient.getQueryState(queryKey)?.isInvalidated).toBe(true);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

test("any other failure shows the server's message and still refetches", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, issues);
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(
    new ApiError(403, { code: "FORBIDDEN", error: "not your project" })
  );
  const hook = renderMoves(queryClient);
  try {
    await act(() => hook.result.current.moveCard("CORE-1", "todo", 1));
    expect(hook.result.current.error).toBe("not your project");
    expect(queryClient.getQueryState(queryKey)?.isInvalidated).toBe(true);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

test("two rapid moves on one board run one after the other, the second from the optimistic state", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, issues);
  const settle: Array<(issue: Issue) => void> = [];
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(
    () =>
      new Promise<Issue>((resolve) => {
        settle.push(resolve);
      })
  );
  const hook = renderMoves(queryClient);
  try {
    let first: Promise<void> = Promise.resolve();
    let second: Promise<void> = Promise.resolve();
    act(() => {
      first = hook.result.current.moveCard("CORE-1", "todo", 1);
      second = hook.result.current.moveCard("CORE-3", "todo", 0);
    });
    // Both optimistic placements are on the board before either request is answered...
    expect(
      (queryClient.getQueryData(queryKey) as IssueSummary[])
        .filter((item) => item.status === "todo")
        .map((item) => item.key)
    ).toEqual(["CORE-3", "CORE-2", "CORE-1"]);
    // ...but only the first PATCH is in flight.
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue.mock.calls[0]).toEqual([
      "CORE-1",
      { rank: { after: "CORE-2" }, status: "todo" },
    ]);
    await act(async () => {
      settle[0]?.(patched);
      await first;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    // The second move's neighbours come from the board as the first move left it.
    expect(patchIssue.mock.calls[1]).toEqual([
      "CORE-3",
      { rank: { before: "CORE-2" }, status: "todo" },
    ]);
    await act(async () => {
      settle[1]?.({ ...patched, key: "CORE-3" });
      await second;
    });
    expect(hook.result.current.error).toBeUndefined();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});
