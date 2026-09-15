import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  fireEvent,
  render,
  renderHook,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Issue, IssueSummary, UserState } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { issueStatuses } from "./board-model";
import { staleBoardMessage, useBoardMoves } from "./board-moves";
import { IssueBoard } from "./IssueBoard";

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <output data-testid="current-route">{location.pathname}</output>;
}

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

function renderBoard(state: UserState = {}, showEdges?: boolean, list: IssueSummary[] = issues) {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(list);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(state);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <KeymapProvider>
          <CurrentRoute />
          <IssueBoard project="CORE" showEdges={showEdges} />
        </KeymapProvider>
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
    expect(card.getAttribute("tabindex")).toBe("-1");
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

// Todo: Alpha, Bravo, Charlie; In progress: Delta - the keyboard rows' seed.
const keyboardIssues: IssueSummary[] = [
  issue({ key: "CORE-1", rank: "a", status: "todo", title: "Alpha" }),
  issue({ key: "CORE-2", rank: "b", status: "todo", title: "Bravo" }),
  issue({ key: "CORE-3", rank: "c", status: "todo", title: "Charlie" }),
  issue({ key: "CORE-4", rank: "d", status: "in_progress", title: "Delta" }),
];

/** A PATCH that never answers: the board stays as the move placed it, so a test reads the
 *  optimistic placement rather than the seed the mocked refetch would hand back. */
function patchInFlight() {
  return spyOn(api, "patchIssue").mockImplementation(() => new Promise<Issue>(() => {}));
}

test("Shift+J moves the focused card down through moveCard, announces the new position and keeps focus on it; Shift+K at the top does nothing", async () => {
  const patchIssue = patchInFlight();
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    const status = within(board).getByRole("status", { name: "Board announcements" });
    expect(status.classList.contains("sr-only")).toBe(true);
    act(() => alpha.focus());
    expect(document.activeElement).toBe(alpha);

    fireEvent.keyDown(alpha, { key: "K", shiftKey: true });
    expect(patchIssue).not.toHaveBeenCalled();
    expect(status.textContent).toBe("");

    await act(async () => {
      fireEvent.keyDown(alpha, { key: "J", shiftKey: true });
    });
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", {
      rank: { after: "CORE-2", before: "CORE-3" },
    });
    expect(status.textContent).toBe("CORE-1 → Todo, position 2 of 3");
    const todo = within(board).getByRole("region", { name: "Todo" });
    await waitFor(() =>
      expect(
        within(todo)
          .getAllByRole("article")
          .map((node) => node.textContent)
      ).toEqual([
        expect.stringContaining("Bravo"),
        expect.stringContaining("Alpha"),
        expect.stringContaining("Charlie"),
      ])
    );
    expect(document.activeElement).toBe(
      within(board).getByRole("article", { name: "CORE-1 Alpha" })
    );
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

test("Shift+L moves the focused card to the top of the next status and focus follows the remounted card", async () => {
  const patchIssue = patchInFlight();
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const bravo = within(board).getByRole("article", { name: "CORE-2 Bravo" });
    act(() => bravo.focus());
    await act(async () => {
      fireEvent.keyDown(bravo, { key: "L", shiftKey: true });
    });
    expect(patchIssue).toHaveBeenCalledWith("CORE-2", {
      rank: { before: "CORE-4" },
      status: "in_progress",
    });
    expect(within(board).getByRole("status", { name: "Board announcements" }).textContent).toBe(
      "CORE-2 → In progress, position 1 of 2"
    );
    const inProgress = within(board).getByRole("region", { name: "In progress" });
    await waitFor(() =>
      expect(
        within(inProgress)
          .getAllByRole("article")
          .map((node) => node.textContent)
      ).toEqual([expect.stringContaining("Bravo"), expect.stringContaining("Delta")])
    );
    expect(document.activeElement).toBe(
      within(inProgress).getByRole("article", { name: "CORE-2 Bravo" })
    );
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

test("j/k/h/l rove real focus over cards, columns and rails", async () => {
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    const bravo = within(board).getByRole("article", { name: "CORE-2 Bravo" });
    const delta = within(board).getByRole("article", { name: "CORE-4 Delta" });
    fireEvent.keyDown(document.body, { key: "j" });
    expect(document.activeElement).toBe(alpha);
    fireEvent.keyDown(alpha, { key: "j" });
    expect(document.activeElement).toBe(bravo);
    fireEvent.keyDown(bravo, { key: "ArrowUp" });
    expect(document.activeElement).toBe(alpha);
    fireEvent.keyDown(alpha, { key: "l" });
    expect(document.activeElement).toBe(delta);
    fireEvent.keyDown(delta, { key: "l" });
    expect(document.activeElement).toBe(within(board).getByRole("region", { name: "Testing" }));
    fireEvent.keyDown(document.activeElement as Element, { key: "h" });
    expect(document.activeElement).toBe(delta);
    for (const _ of ["testing", "needs_review", "retro", "done"]) {
      fireEvent.keyDown(document.activeElement as Element, { key: "l" });
    }
    const doneRail = within(board).getByRole("region", { name: "Done (collapsed)" });
    expect(document.activeElement).toBe(doneRail);
    fireEvent.keyDown(doneRail, { key: "l" });
    expect(document.activeElement).toBe(doneRail);
    fireEvent.keyDown(doneRail, { key: "Escape" });
    expect(document.activeElement).toBe(document.body);
    fireEvent.keyDown(document.body, { key: "k" });
    expect(document.activeElement).toBe(delta);
  } finally {
    cleanup();
  }
});

test("Enter and o open the focused card once, p focuses its priority select, Escape returns to the card and then out", async () => {
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    const link = within(alpha).getByRole("link");
    let clicks = 0;
    link.addEventListener("click", () => (clicks += 1));
    act(() => alpha.focus());

    fireEvent.keyDown(alpha, { key: "p" });
    const select = within(alpha).getByRole("combobox", { name: "Priority of CORE-1" });
    expect(document.activeElement).toBe(select);
    // Single letters never fire on an editable target.
    fireEvent.keyDown(select, { key: "j" });
    expect(document.activeElement).toBe(select);
    fireEvent.keyDown(select, { key: "Escape" });
    expect(document.activeElement).toBe(alpha);
    fireEvent.keyDown(alpha, { key: "Escape" });
    expect(document.activeElement).toBe(document.body);

    act(() => alpha.focus());
    fireEvent.keyDown(alpha, { key: "Enter" });
    expect(clicks).toBe(1);
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1");
  } finally {
    cleanup();
  }
});

test("Space on a focused card lifts nothing, and the card's instructions name the registry keys, not dnd-kit's grammar", async () => {
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue(patched);
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    act(() => alpha.focus());
    fireEvent.keyDown(alpha, { key: " ", code: "Space" });
    fireEvent.keyDown(alpha, { key: "ArrowDown" });
    fireEvent.keyDown(document.activeElement as Element, { key: " ", code: "Space" });
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();
    expect(board.querySelector(".opacity-50")).toBeNull();
    const describedBy = alpha.getAttribute("aria-describedby");
    expect(describedBy).not.toBeNull();
    const instructions = document.getElementById(describedBy as string)?.textContent ?? "";
    expect(instructions).toContain("Shift with J, K, H or L");
    expect(instructions).not.toContain("space bar");
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});
