import { expect, mock, spyOn, test } from "bun:test";
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
function CachedControl({ stopPropagation }: { stopPropagation?: boolean }): ReactNode {
  const cached = useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) {
    return null;
  }
  return (
    <PriorityControl
      issueKey={cached.data.key}
      priority={cached.data.priority}
      stopPropagation={stopPropagation}
    />
  );
}

function renderControl(children: (control: ReactNode) => ReactNode = (control) => control) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(detailKey, details);
  queryClient.setQueryData(listKey, [summary, otherSummary]);
  queryClient.setQueryData(inboxKey, [inboxRow]);
  const view = render(
    <QueryClientProvider client={queryClient}>{children(<CachedControl />)}</QueryClientProvider>
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

test("PriorityControl saves the picked priority and shows it before the server answers", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(() => save.promise);
  const { queryClient, unmount } = renderControl();
  try {
    fireEvent.change(select(), { target: { value: "2" } });
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }));
    expect(select().value).toBe("2");
    expect(screen.getByText("P2", { selector: "span" })).toBeTruthy();
    expect(select().disabled).toBe(true);
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 2 },
      otherSummary,
    ]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([{ ...inboxRow, priority: 2 }]);

    await act(async () => {
      save.resolve({ ...issue, priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(select().disabled).toBe(false));
    expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2);
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

test("PriorityControl ignores a second pick while the first save is in flight", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(() => save.promise);
  const { unmount } = renderControl();
  try {
    fireEvent.change(select(), { target: { value: "0" } });
    fireEvent.change(select(), { target: { value: "3" } });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 });
    await act(async () => {
      save.resolve({ ...issue, priority: 0 });
      await save.promise;
    });
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl with stopPropagation keeps its clicks, taps and keys from the card", () => {
  const onClick = mock(() => {});
  const onPointerDown = mock(() => {});
  const onKeyDown = mock(() => {});
  const { unmount } = renderControl(() => (
    // biome-ignore lint/a11y/useSemanticElements: stands in for a card whose React handlers would navigate or drag
    <div
      onClick={onClick}
      onKeyDown={onKeyDown}
      onPointerDown={onPointerDown}
      role="link"
      tabIndex={0}
    >
      <CachedControl stopPropagation />
    </div>
  ));
  try {
    fireEvent.pointerDown(select());
    fireEvent.click(select());
    fireEvent.keyDown(select(), { key: "Enter" });
    expect(onPointerDown).not.toHaveBeenCalled();
    expect(onClick).not.toHaveBeenCalled();
    expect(onKeyDown).not.toHaveBeenCalled();
  } finally {
    unmount();
  }
});
