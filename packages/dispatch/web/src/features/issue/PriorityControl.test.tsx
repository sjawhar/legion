import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { InboxRow, Issue, IssueDetails, IssueSummary } from "../../api/types";
import { PriorityControl } from "./PriorityControl";

const issue: Issue = {
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  assignee: null,
  primary_artifact_id: "artifact-1",
  priority: 1,
  project: "CORE",
  rank: "U",
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};
const details: IssueDetails = { ...issue, artifacts: [], children: [], open_asks: [] };
const summary: IssueSummary = {
  key: issue.key,
  labels: [],
  last_seq: 1,
  open_asks: 0,
  parent: null,
  assignee: null,
  priority: issue.priority,
  rank: "U",
  status: issue.status,
  title: issue.title,
  updated_at: issue.updated_at,
};
const otherSummary: IssueSummary = { ...summary, key: "CORE-2", priority: 3 };
const inboxRow: InboxRow = {
  anchor: null,
  answer: null,
  author: { id: "session-1", kind: "session" },
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: issue.key,
  kind: "action",
  multiple: false,
  opened_event_id: 1,
  options: [],
  priority: issue.priority,
  question: "Which layout?",
  state: "open",
  urgency: "med",
};

const detailKey = ["issue", issue.key] as const;
const listKey = ["issues", "project", issue.project] as const;
const inboxKey = ["inbox"] as const;

/** Renders the control the way every surface does: with the priority it reads from the cache. */
function CachedControl(): ReactNode {
  const cached = useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) {
    return null;
  }
  return <PriorityControl issueKey={cached.data.key} priority={cached.data.priority} />;
}

function renderControl() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(detailKey, details);
  queryClient.setQueryData(listKey, [summary, otherSummary]);
  queryClient.setQueryData(inboxKey, [inboxRow]);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <CachedControl />
    </QueryClientProvider>
  );
  return { queryClient, unmount: view.unmount };
}

function select(): HTMLSelectElement {
  return screen.getByRole("combobox", { name: "Priority of CORE-1" }) as HTMLSelectElement;
}

test("PriorityControl shows the issue's priority and offers Unset and P0-P3", () => {
  const { unmount } = renderControl();
  try {
    expect(screen.getByText("P1", { selector: "span" })).toBeTruthy();
    expect(select().value).toBe("1");
    expect([...select().options].map((option) => option.value)).toEqual(["", "0", "1", "2", "3"]);
  } finally {
    unmount();
  }
});

test("PriorityControl saves the picked priority, shows it before the server answers, and keeps focus", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(() => save.promise);
  const { queryClient, unmount } = renderControl();
  try {
    select().focus();
    fireEvent.change(select(), { target: { value: "2" } });
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }));
    expect(select().value).toBe("2");
    expect(screen.getByText("P2", { selector: "span" })).toBeTruthy();
    // A disabled control drops focus and leaves the tab order for the length of the save, so
    // the picker stays enabled while the request is in flight.
    expect(select().disabled).toBe(false);
    expect(document.activeElement).toBe(select());
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 2 },
      otherSummary,
    ]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([{ ...inboxRow, priority: 2 }]);

    await act(async () => {
      save.resolve({ ...issue, priority: 2 });
      await save.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2)
    );
    expect(document.activeElement).toBe(select());
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl sends null for Unset and shows the muted tag", async () => {
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue({ ...issue, priority: null });
  const { unmount } = renderControl();
  try {
    fireEvent.change(select(), { target: { value: "" } });
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: null }));
    expect(select().value).toBe("");
    expect(screen.getByText("Priority", { selector: "span" })).toBeTruthy();
    expect(screen.queryByText("P1", { selector: "span" })).toBeNull();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl rolls every surface back and offers Retry when the save fails", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(() => save.promise);
  const { queryClient, unmount } = renderControl();
  try {
    fireEvent.change(select(), { target: { value: "0" } });
    await waitFor(() => expect(select().value).toBe("0"));

    act(() => {
      save.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(select().value).toBe("1");
    expect(select().disabled).toBe(false);
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([summary, otherSummary]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([inboxRow]);

    patchIssue.mockResolvedValue({ ...issue, priority: 0 });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 });
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(select().value).toBe("0");
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl saves a pick made during the first save once that save settles, newest first shown", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = spyOn(api, "patchIssue")
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount } = renderControl();
  try {
    fireEvent.change(select(), { target: { value: "0" } });
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }));
    fireEvent.change(select(), { target: { value: "3" } });
    fireEvent.change(select(), { target: { value: "2" } });
    // One request at a time: the later picks wait for the first answer, but the badge already
    // shows the newest one and never snaps back to the value still being saved.
    expect(select().value).toBe("2");
    expect(screen.getByText("P2", { selector: "span" })).toBeTruthy();
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 });
    expect(select().value).toBe("2");

    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2)
    );
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("a queued pick that fails rolls back to the priority the server last confirmed, not the pick", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = spyOn(api, "patchIssue")
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount } = renderControl();
  try {
    fireEvent.change(select(), { target: { value: "0" } });
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }));
    fireEvent.change(select(), { target: { value: "2" } });
    expect(select().value).toBe("2");

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }));
    act(() => {
      second.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(select().value).toBe("0");
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 0 },
      otherSummary,
    ]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([{ ...inboxRow, priority: 0 }]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl keeps mouse, pointer and touch presses on the select out of a draggable parent", () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const reached: string[] = [];
  const view = render(
    <QueryClientProvider client={queryClient}>
      <article
        aria-label="CORE-1 Review the spec"
        onMouseDown={() => reached.push("mousedown")}
        onPointerDown={() => reached.push("pointerdown")}
        onTouchStart={() => reached.push("touchstart")}
      >
        <PriorityControl issueKey="CORE-1" priority={1} />
      </article>
    </QueryClientProvider>
  );
  try {
    fireEvent.mouseDown(select());
    fireEvent.pointerDown(select());
    fireEvent.touchStart(select());
    expect(reached).toEqual([]);
  } finally {
    view.unmount();
  }
});
