import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import type { InboxRow, Issue, IssueDetails, IssueSummary } from "../../api/types";
import { AssigneeControl } from "./AssigneeControl";

const issue: Issue = {
  assignee: "alice",
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
  priority: null,
  project: "CORE",
  rank: "U",
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};
const details: IssueDetails = { ...issue, artifacts: [], children: [], open_asks: [] };
const summary: IssueSummary = {
  assignee: issue.assignee,
  key: issue.key,
  labels: [],
  last_seq: 1,
  open_asks: 0,
  parent: null,
  priority: null,
  rank: "U",
  status: issue.status,
  title: issue.title,
  updated_at: issue.updated_at,
};
const inboxRow: InboxRow = {
  anchor: null,
  answer: null,
  author: { id: "session-1", kind: "session" },
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue: { assignee: issue.assignee, key: issue.key, title: issue.title },
  issue_key: issue.key,
  kind: "action",
  multiple: false,
  opened_event_id: 1,
  options: [],
  priority: null,
  question: "Which layout?",
  state: "open",
  urgency: "med",
};

const detailKey = ["issue", issue.key] as const;
const listKey = ["issues", "project", issue.project] as const;
const inboxKey = ["inbox"] as const;

function CachedControl(): ReactNode {
  const cached = useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) {
    return null;
  }
  return <AssigneeControl assignee={cached.data.assignee} issueKey={cached.data.key} />;
}

function renderControl() {
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(detailKey, details);
  queryClient.setQueryData(listKey, [summary]);
  queryClient.setQueryData(inboxKey, [inboxRow]);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <CachedControl />
    </QueryClientProvider>
  );
  return {
    listUsers,
    queryClient,
    unmount: () => {
      view.unmount();
      listUsers.mockRestore();
    },
  };
}

function select(): HTMLSelectElement {
  return screen.getByRole("combobox", { name: "Assignee of CORE-1" }) as HTMLSelectElement;
}

test("AssigneeControl reads the allowlist only once the reader reaches for it, then lists Unassigned first", async () => {
  const { listUsers, unmount } = renderControl();
  try {
    // Mounted with the badge showing, nothing has been requested: a fresh issue load costs no
    // allowlist read.
    expect(screen.getByText("alice", { selector: "span" })).toBeTruthy();
    expect([...select().options].map((option) => option.textContent)).toEqual([
      "Unassigned",
      "alice",
    ]);
    expect(listUsers).not.toHaveBeenCalled();

    fireEvent.focus(select());
    await waitFor(() =>
      expect([...select().options].map((option) => option.textContent)).toEqual([
        "Unassigned",
        "alice",
        "bob",
      ])
    );
    expect(listUsers).toHaveBeenCalledTimes(1);
    expect(select().value).toBe("alice");
  } finally {
    unmount();
  }
});

test("AssigneeControl reassigns optimistically on every surface and merges the server's answer", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(() => save.promise);
  const { queryClient, unmount } = renderControl();
  try {
    fireEvent.pointerEnter(select());
    await waitFor(() => expect(select().options.length).toBe(3));
    fireEvent.change(select(), { target: { value: "bob" } });
    await waitFor(() => expect(select().value).toBe("bob"));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: "bob" });
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)?.[0]?.assignee).toBe("bob");
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)?.[0]?.issue?.assignee).toBe("bob");

    act(() => {
      save.resolve({ ...issue, assignee: "bob", last_seq: 2 });
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("AssigneeControl sends null for Unassigned", async () => {
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue({ ...issue, assignee: null });
  const { unmount } = renderControl();
  try {
    fireEvent.focus(select());
    await waitFor(() => expect(select().options.length).toBe(3));
    fireEvent.change(select(), { target: { value: "" } });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: null }));
    await waitFor(() => expect(select().value).toBe(""));
    expect(screen.getByText("Unassigned", { selector: "span" })).toBeTruthy();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("AssigneeControl rolls back and shows the server's refusal when the login is not allowed", async () => {
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(
    new ApiError(400, {
      code: "ASSIGNEE_NOT_ALLOWED",
      error: '"bob" is not a login on the sign-in allowlist',
    })
  );
  const { queryClient, unmount } = renderControl();
  try {
    fireEvent.focus(select());
    await waitFor(() => expect(select().options.length).toBe(3));
    fireEvent.change(select(), { target: { value: "bob" } });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain('"bob" is not a login on the sign-in allowlist');
    expect(select().value).toBe("alice");
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([summary]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([inboxRow]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});
