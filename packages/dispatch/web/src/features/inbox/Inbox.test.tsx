import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerAskSnoozes, answerIssuePatches } from "../../__tests__/send-answers";
import { wholeText } from "../../__tests__/whole-text";
import { ApiError, api } from "../../api/client";
import type {
  Artifact,
  AskRead,
  Comment,
  CredentialPendingRow,
  InboxRow,
  Issue,
  IssueDetails,
} from "../../api/types";
import { DocumentRuntime, type DocumentRuntimeValue } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DIALOG_SCOPE } from "../shell/keymap";
import { Inbox } from "./Inbox";
import { installInboxApiMocks, issueAsk, mockAskReads } from "./inbox-fixture";

const mocks = installInboxApiMocks();

function artifactAsk(): InboxRow {
  return {
    anchor: null,
    answer: null,
    artifact_id: "artifact-1",
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    document: { name: "Design notes", project: "CORE", slug: "design-notes" },
    edited_at: null,
    id: "ask-1",
    issue_key: null,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    thread: {
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies: [],
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
    },
    question: "Does this design need review?",
    priority: null,
    snoozed_until: null,
    state: "open",
    waiting_on: "human",
    urgency: "med",
  };
}

function inboxIssue(
  id: string,
  key: string,
  question: string,
  overrides: Partial<InboxRow> = {}
): InboxRow {
  return issueAsk({
    id,
    issue: { assignee: "alice", key, title: `${key} title` },
    issue_key: key,
    question,
    ...overrides,
  });
}

/** The narrow `Issue` a PATCH answers with, for an inbox row's issue. */
function narrowIssueOf(row: InboxRow, assignee: string | null): Issue {
  return {
    assignee,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    closed_at: null,
    created_at: row.created_at,
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: row.issue_key ?? "",
    labels: [],
    last_seq: 2,
    number: 3,
    parent: null,
    blocked_by: [],
    primary_artifact_id: "artifact-3",
    priority: null,
    project: "CORE",
    rank: "U",
    route: null,
    repo: null,
    resolved_repo: null,
    status: "todo",
    title: row.issue?.title ?? "",
    updated_at: row.created_at,
  };
}

test("a cold Inbox hydrates every ask thread from its one list response", async () => {
  const rows = Array.from({ length: 20 }, (_, index) => {
    const row = issueAsk({
      id: `ask-${index + 1}`,
      question: `Question ${index + 1}`,
    });
    return {
      ...row,
      thread: {
        answers: [],
        edits: [],
        claimant: null,
        followers: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
        replies: [
          {
            ...commentDeliveryFields(),
            anchor: null,
            ask_id: row.id,
            author: { id: "session-1", kind: "session" as const },
            body: `Reply ${index + 1}`,
            created_at: "2026-09-11T01:00:00Z",
            edited_at: null,
            id: `reply-${index + 1}`,
            issue_key: row.issue_key,
            reply_to: null,
            resolved: false,
            resolved_at: null,
            resolved_by: null,
            suggestion: null,
            turn: "agent" as const,
          },
        ],
      },
    };
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = spyOn(api, "getAsk").mockRejectedValue(new Error("per-row fetch"));
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() =>
      expect(
        rows.map(
          (row) => queryClient.getQueryData<AskRead>(["ask-thread", row.id])?.replies[0]?.body
        )
      ).toEqual(rows.map((row) => row.thread.replies[0]?.body))
    );
    expect(getInbox).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(getAsk).toHaveBeenCalledTimes(0));
    await waitFor(() => expect(mocks.getReferences).toHaveBeenCalledTimes(0));
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an unchanged old Inbox cache does not refetch every thread on remount", async () => {
  const row = issueAsk({
    id: "ask-old",
    thread: {
      answers: [],
      edits: [],
      claimant: null,
      followers: [],
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
      replies: [
        {
          ...commentDeliveryFields(),
          anchor: null,
          ask_id: "ask-old",
          author: { id: "session-1", kind: "session" },
          body: "Still current.",
          created_at: "2026-09-11T01:00:00Z",
          edited_at: null,
          id: "reply-old",
          issue_key: "CORE-1",
          reply_to: null,
          resolved: false,
          resolved_at: null,
          resolved_by: null,
          suggestion: null,
          turn: "agent",
        },
      ],
    },
  });
  const getInbox = spyOn(api, "getInbox").mockImplementation(
    () => new Promise<InboxRow[]>(() => {})
  );
  const getAsk = spyOn(api, "getAsk").mockRejectedValue(new Error("old cache must not refresh"));
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(["inbox"], [row], { updatedAt: Date.now() - 30_001 });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Still current.");
    await waitFor(() => expect(getAsk).toHaveBeenCalledTimes(0));
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox labels an artifact-owned ask with its project and document page link", async () => {
  const ask = artifactAsk();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([ask]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // A document ask belongs to nobody, so the reader's own list is not where it shows.
  const view = render(
    <MemoryRouter initialEntries={["/?assignee=unassigned"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("CORE · Design notes");
    expect(screen.getByRole("link", { name: "CORE · Design notes" }).getAttribute("href")).toBe(
      "/projects/CORE/documents/design-notes?ask=ask-1"
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox puts every ask waiting on the viewer under Waiting on you", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which approach?");
    expect(
      screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)
    ).toEqual(["Waiting on you"]);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox keeps rows waiting on agents below Waiting on you without duplicating rows", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T01:00:00Z" },

    waiting_on: "agent",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which approach?");
    const headings = screen
      .getAllByRole("heading", { level: 2 })
      .map((heading) => heading.textContent);
    expect(headings).toEqual(["Waiting on you", "Waiting on agents"]);
    expect(screen.getByText(wholeText("Waiting on session:session-…"))).toBeTruthy();
    // No `?agent=`, so no agent filter to clear.
    expect(screen.queryByRole("link", { name: "Clear agent filter" })).toBeNull();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox keeps an agent's latest reply on its Waiting-on-you row", async () => {
  const askA = issueAsk({
    id: "ask-a",
    last_reply: {
      author: { id: "session-2", kind: "session" },
      created_at: "2026-09-11T01:00:00Z",
    },
  });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T02:00:00Z" },

    waiting_on: "agent",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which approach?");
    expect(screen.getByText(wholeText("session:session-… replied"))).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox partitions by waiting_on: an agent's progress note keeps its ask under Waiting on agents", async () => {
  const noted = issueAsk({
    id: "ask-noted",
    last_reply: {
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-11T01:00:00Z",
    },
    question: "Which auditor?",
    waiting_on: "agent",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([noted]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: noted,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which auditor?");
    const headings = screen
      .getAllByRole("heading", { level: 2 })
      .map((heading) => heading.textContent);
    expect(headings).toEqual(["Waiting on agents"]);
    expect(screen.queryByText(wholeText("session:session-… replied"))).toBeNull();
    expect(screen.getByText(wholeText("Waiting on session:session-…"))).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox preserves server priority order within Waiting on you", async () => {
  const p2 = issueAsk({
    created_at: new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString(),
    id: "ask-p2",
    priority: 2,
    question: "P2 earlier action",
  });
  const p0 = issueAsk({
    created_at: new Date(Date.now() - 35 * 60 * 1000).toISOString(),
    id: "ask-p0",
    priority: 0,
    question: "P0 later action",
  });
  const agentWaits = issueAsk({
    id: "ask-agent",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: new Date().toISOString() },

    waiting_on: "agent",
    question: "Waiting on agent",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([p0, agentWaits, p2]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: [p0, p2, agentWaits].find((ask) => ask.id === id) ?? p0,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByRole("heading", { name: "Waiting on you" });
    expect(
      screen
        .getAllByTestId(/^ask-ask-/)
        .filter(
          (card) =>
            card.closest("[data-inbox-section]")?.getAttribute("data-inbox-section") === "human"
        )
        .map((card) => card.dataset.testid)
    ).toEqual(["ask-ask-p0", "ask-ask-p2"]);
    expect(screen.getByText("Blocked on you: 2 items, oldest 2d")).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }

  const emptyInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const emptyView = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const emptyState = await screen.findByRole("region", { name: "Inbox empty state" });
    expect(within(emptyState).getByText("Nothing needs you")).toBeTruthy();
    expect(screen.queryByText(/Blocked on you:/)).toBeNull();
  } finally {
    emptyView.unmount();
    emptyInbox.mockRestore();
  }
});

test("Inbox groups one issue's interleaved asks under a header at the first ask", async () => {
  const first = inboxIssue("ask-first", "CORE-1", "First question");
  const other = inboxIssue("ask-other", "CORE-2", "Other question");
  const second = inboxIssue("ask-second", "CORE-1", "Second question");
  const third = inboxIssue("ask-third", "CORE-1", "Third question");
  const rows = [first, other, second, third];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("ask-ask-first");
    const header = document.querySelector<HTMLElement>('[data-inbox-group-header="issue:CORE-1"]');
    if (header === null) throw new Error("CORE-1 group header missing");
    expect(header.textContent).toContain("CORE-1");
    expect(header.textContent).toContain("CORE-1 title");
    expect(header.textContent).toContain("3 asks");
    expect(rowIds()).toEqual(["ask-first", "ask-second", "ask-third", "ask-other"]);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("j enters a group at its first row instead of its header", async () => {
  const above = inboxIssue("ask-above", "CORE-9", "Above");
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const second = inboxIssue("ask-second", "CORE-1", "Second");
  const rows = [above, first, second];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("ask-ask-first");
    const header = document.querySelector<HTMLElement>('[data-inbox-group-header="issue:CORE-1"]');
    if (header === null) throw new Error("CORE-1 group header missing");
    const aboveRow = screen.getByTestId("ask-ask-above").closest<HTMLElement>("[data-inbox-row]");
    const firstRow = screen.getByTestId("ask-ask-first").closest<HTMLElement>("[data-inbox-row]");
    if (aboveRow === null || firstRow === null) throw new Error("Inbox row missing");

    fireEvent.keyDown(document.body, { key: "j" });
    expect(document.activeElement).toBe(aboveRow);
    fireEvent.keyDown(aboveRow, { key: "j" });
    expect(document.activeElement).toBe(firstRow);
    expect(document.activeElement).not.toBe(header);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("k enters a group at its last row instead of its header", async () => {
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const second = inboxIssue("ask-second", "CORE-1", "Second");
  const below = inboxIssue("ask-below", "CORE-9", "Below");
  const rows = [first, second, below];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("ask-ask-first");
    const header = document.querySelector<HTMLElement>('[data-inbox-group-header="issue:CORE-1"]');
    if (header === null) throw new Error("CORE-1 group header missing");
    const belowRow = screen.getByTestId("ask-ask-below").closest<HTMLElement>("[data-inbox-row]");
    const lastGroupRow = screen
      .getByTestId("ask-ask-second")
      .closest<HTMLElement>("[data-inbox-row]");
    if (belowRow === null || lastGroupRow === null) throw new Error("Inbox row missing");

    fireEvent.keyDown(document.body, { key: "k" });
    expect(document.activeElement).toBe(belowRow);
    fireEvent.keyDown(belowRow, { key: "k" });
    expect(document.activeElement).toBe(lastGroupRow);
    expect(document.activeElement).not.toBe(header);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a group header names asks in another band and focuses that group's first row", async () => {
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const second = inboxIssue("ask-second", "CORE-1", "Second");
  const agentFirst = inboxIssue("ask-agent-first", "CORE-1", "Agent first", {
    waiting_on: "agent",
  });
  const agentSecond = inboxIssue("ask-agent-second", "CORE-1", "Agent second", {
    waiting_on: "agent",
  });
  const rows = [first, second, agentFirst, agentSecond];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const previousScrollIntoView = HTMLElement.prototype.scrollIntoView;
  HTMLElement.prototype.scrollIntoView = () => {};
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("ask-ask-first");
    const header = document.querySelector<HTMLElement>('[data-inbox-group-header="issue:CORE-1"]');
    if (header === null) throw new Error("CORE-1 group header missing");
    const jump = within(header).getByRole("button", {
      name: "Jump to CORE-1's asks Waiting on agents",
    });
    expect(jump.textContent).toBe("2 more waiting on agents");

    fireEvent.click(jump);
    const target = screen
      .getByTestId("ask-ask-agent-first")
      .closest<HTMLElement>("[data-inbox-row]");
    if (target === null) throw new Error("target row missing");
    expect(document.activeElement).toBe(target);
  } finally {
    unmount();
    HTMLElement.prototype.scrollIntoView = previousScrollIntoView;
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a group header can focus an ungrouped singleton in another band", async () => {
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const second = inboxIssue("ask-second", "CORE-1", "Second");
  const targetAsk = inboxIssue("ask-agent", "CORE-1", "Agent only", { waiting_on: "agent" });
  const rows = [first, second, targetAsk];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const previousScrollIntoView = HTMLElement.prototype.scrollIntoView;
  HTMLElement.prototype.scrollIntoView = () => {};
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("ask-ask-first");
    const header = document.querySelector<HTMLElement>('[data-inbox-group-header="issue:CORE-1"]');
    if (header === null) throw new Error("CORE-1 group header missing");
    fireEvent.click(
      within(header).getByRole("button", { name: "Jump to CORE-1's asks Waiting on agents" })
    );
    const target = screen.getByTestId("ask-ask-agent").closest<HTMLElement>("[data-inbox-row]");
    if (target === null) throw new Error("target row missing");
    expect(target.getAttribute("data-inbox-group")).toBeNull();
    expect(document.activeElement).toBe(target);
  } finally {
    unmount();
    HTMLElement.prototype.scrollIntoView = previousScrollIntoView;
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a group header opens Later before focusing its target row", async () => {
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const second = inboxIssue("ask-second", "CORE-1", "Second");
  const later = inboxIssue("ask-later", "CORE-1", "Later", {
    snoozed_until: new Date(Date.now() + 60_000).toISOString(),
  });
  const rows = [first, second, later];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const previousScrollIntoView = HTMLElement.prototype.scrollIntoView;
  HTMLElement.prototype.scrollIntoView = () => {};
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("ask-ask-first");
    const header = document.querySelector<HTMLElement>('[data-inbox-group-header="issue:CORE-1"]');
    if (header === null) throw new Error("CORE-1 group header missing");
    fireEvent.click(within(header).getByRole("button", { name: "Jump to CORE-1's asks Later" }));
    expect(screen.getByRole("button", { name: "Later (1)" }).getAttribute("aria-expanded")).toBe(
      "true"
    );
    const target = screen.getByTestId("ask-ask-later").closest<HTMLElement>("[data-inbox-row]");
    if (target === null) throw new Error("target row missing");
    expect(document.activeElement).toBe(target);
  } finally {
    unmount();
    HTMLElement.prototype.scrollIntoView = previousScrollIntoView;
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a held row stays inside its owner's group", async () => {
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const held = inboxIssue("ask-held", "CORE-1", "Held");
  const other = inboxIssue("ask-other", "CORE-2", "Other");
  const urgent = inboxIssue("ask-urgent", "CORE-3", "Urgent", { priority: 0 });
  const rows = [first, held, other];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads([first, held, other, urgent]);
  const { queryClient, unmount } = renderInbox();

  try {
    const heldCard = await screen.findByTestId("ask-ask-held");
    const heldRow = heldCard.closest<HTMLElement>("[data-inbox-row]");
    const firstRow = screen.getByTestId("ask-ask-first").closest<HTMLElement>("[data-inbox-row]");
    if (heldRow === null || firstRow === null) throw new Error("Inbox row missing");
    fireEvent.pointerOver(heldRow);

    act(() => {
      queryClient.setQueryData<InboxRow[]>(
        ["inbox"],
        [
          urgent,
          first,
          {
            ...held,
            last_reply: {
              author: { id: "session-1", kind: "session" },
              created_at: held.created_at,
            },
            waiting_on: "agent",
          },
          other,
        ]
      );
    });

    const header = document.querySelector<HTMLElement>('[data-inbox-group-header="issue:CORE-1"]');
    if (header === null) throw new Error("CORE-1 group header missing");
    expect(heldRow.getAttribute("data-inbox-group")).toBe("issue:CORE-1");
    expect(firstRow.previousElementSibling).toBe(header);
    expect(heldRow.previousElementSibling).toBe(firstRow);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

function secretRequest(overrides: Partial<CredentialPendingRow> = {}): CredentialPendingRow {
  return {
    identifiers: ["DEMO_API_KEY"],
    kind: "agent_secret",
    record_id: "record-1",
    requested_at: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
    ...overrides,
  };
}

test("a credential request alone is listed and on the banner, and nothing says nothing needs you", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  mocks.getCredentialPending.mockResolvedValue({ pending: [secretRequest()] });
  const { unmount } = renderInbox();
  try {
    const request = await screen.findByRole("link", { name: /Secret request.*DEMO_API_KEY/s });
    expect(request.getAttribute("href")).toBe("/credentials/record-1");
    expect(screen.getByText("Blocked on you: 1 item, oldest 3h")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Inbox empty state" })).toBeNull();
    expect(screen.queryByText("Nothing needs you")).toBeNull();
  } finally {
    unmount();
    getInbox.mockRestore();
  }
});

test("the banner counts credential requests beside the asks waiting on you, oldest of either", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([
    issueAsk({ created_at: new Date().toISOString() }),
  ]);
  mocks.getCredentialPending.mockResolvedValue({
    pending: [
      secretRequest({ requested_at: new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString() }),
    ],
  });
  const { unmount } = renderInbox();
  try {
    await screen.findByRole("link", { name: /Secret request.*DEMO_API_KEY/s });
    expect(await screen.findByText("Blocked on you: 2 items, oldest 2d")).toBeTruthy();
  } finally {
    unmount();
    getInbox.mockRestore();
  }
});

test("a credential list that fails to load keeps the Inbox from saying nothing needs you", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  mocks.getCredentialPending.mockRejectedValue(
    new ApiError(503, { code: "AGENT_SECRETS_UNAVAILABLE" })
  );
  const { unmount } = renderInbox();
  try {
    expect(await screen.findByText("Couldn't load credential requests.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Inbox empty state" })).toBeNull();
  } finally {
    unmount();
    getInbox.mockRestore();
  }
});

test("Inbox narrows to one agent's asks from ?agent and clears back to the whole inbox", async () => {
  const fromPlanner = issueAsk({ id: "ask-planner", question: "Planner question" });
  const plannerWaits = issueAsk({
    id: "ask-planner-waits",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: new Date().toISOString() },

    waiting_on: "agent",
    question: "Planner waits on alice",
  });
  const fromReviewer = issueAsk({
    author: { id: "reviewer-session", kind: "session", origin: { session_title: "Reviewer" } },
    id: "ask-reviewer",
    question: "Reviewer question",
  });
  const rows = [fromPlanner, plannerWaits, fromReviewer];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: rows.find((ask) => ask.id === id) ?? fromPlanner,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/w",
      host: "m",
      last_activity: null,
      last_seen: Date.now(),
      machine_id: "m",
      open_asks: 2,
      roles: [],
      session_id: "session-1",
      title: "Planner",
    },
  ]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?agent=session-1"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Planner question");
    expect(screen.getByText("Planner waits on alice")).toBeTruthy();
    expect(screen.queryByText("Reviewer question")).toBeNull();
    expect(
      screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)
    ).toEqual(["Waiting on you", "Waiting on agents"]);
    const chip = await screen.findByRole("link", { name: "Clear agent filter" });
    expect(chip.textContent).toBe("Asks from Planner · clear");
    expect(chip.getAttribute("href")).toBe("/");
    // The agent's live link sits beside the clear-filter link, never inside it.
    const chipAgent = chip.closest<HTMLElement>("[data-agent-name]");
    if (chipAgent === null) throw new Error("the agent filter chip is not drawn by AgentName");
    const live = await within(chipAgent).findByRole("link", {
      name: "Open live conversation with Planner",
    });
    expect(live.getAttribute("href")).toBe("/agents/session-1/live");
    expect(chip.contains(live)).toBe(false);
    // The whole-inbox banner would contradict the filtered list (and its link clears the filter).
    expect(screen.queryByText(/Blocked on you:/)).toBeNull();
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox ?section=needs-you keeps only the agent's asks waiting on the viewer", async () => {
  const fromPlanner = issueAsk({ id: "ask-planner", question: "Planner question" });
  const plannerWaits = issueAsk({
    id: "ask-planner-waits",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: new Date().toISOString() },

    waiting_on: "agent",
    question: "Planner waits on alice",
  });
  const rows = [fromPlanner, plannerWaits];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: rows.find((ask) => ask.id === id) ?? fromPlanner,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?agent=session-1&section=needs-you"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Planner question");
    expect(screen.queryByText("Planner waits on alice")).toBeNull();
    expect(
      screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)
    ).toEqual(["Waiting on you"]);
    // No live agent carries the title, so the chip falls back to the asks' author label.
    expect(screen.getByRole("link", { name: "Clear agent filter" }).textContent).toBe(
      "Asks from session:session-… waiting on you · clear"
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox filtered to an agent with no open asks says so and still offers to clear", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: issueAsk(),
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  });
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = render(
    <MemoryRouter initialEntries={["/?agent=idle-session"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const emptyState = await screen.findByRole("region", { name: "Inbox empty state" });
    // A session the registry does not list and no listed ask names reads by its short id.
    expect(within(emptyState).getByText("No open asks from session:idle-ses…")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Clear agent filter" }).getAttribute("href")).toBe("/");
    expect(screen.queryByText("Which approach?")).toBeNull();
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a row being typed into stays under Waiting on you when an agent's note flips its turn, keeps its draft, and moves once the reader leaves it", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which format?",
  });
  const askC = issueAsk({
    id: "ask-c",
    issue: { assignee: "alice", key: "CORE-3", title: "Third issue" },
    issue_key: "CORE-3",
    priority: 0,
    question: "Brand new ask",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: [askA, askB, askC].find((ask) => ask.id === id) ?? askA,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    fireEvent.click(await within(card).findByRole("radio", { name: "Ship" }));
    const field = within(card).getByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "Ship it after the audit" } });
    field.focus();

    // An agent's progress note hands the turn to the agent while a P0 ask arrives above: the row
    // the reader is typing into stays where they see it, with the new turn on its card.
    act(() => {
      queryClient.setQueryData<InboxRow[]>(
        ["inbox"],
        [
          askC,
          askA,
          {
            ...askB,
            last_reply: {
              author: { id: "session-1", kind: "session" },
              created_at: "2026-09-11T01:00:00Z",
            },
            waiting_on: "agent",
          },
        ]
      );
    });

    const moved = screen.getByTestId("ask-ask-b");
    expect(within(moved).getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe(
      "Ship it after the audit"
    );
    expect(within(moved).getByRole<HTMLInputElement>("radio", { name: "Ship" }).checked).toBe(true);
    expect(document.activeElement).toBe(field);
    expect(moved).toBe(card);
    expect(headings()).toEqual(["Waiting on you"]);
    expect(rowIds()).toEqual(["ask-c", "ask-a", "ask-b"]);
    expect(moved.closest("[data-inbox-section]")?.getAttribute("data-inbox-section")).toBe("human");
    expect(screen.getByTestId("turn-ask-b").textContent).toBe("Waiting on session:session-…");

    // Their focus leaves the row: it takes its place under Waiting on agents, draft intact.
    act(() => field.blur());
    await waitFor(() => expect(headings()).toEqual(["Waiting on you", "Waiting on agents"]));
    expect(moved.closest("[data-inbox-section]")?.getAttribute("data-inbox-section")).toBe("agent");
    expect(within(moved).getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe(
      "Ship it after the audit"
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the reader's own Ask back keeps the row under Waiting on you, unscrolled, until they leave it", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const askC = issueAsk({
    id: "ask-c",
    issue: { assignee: "alice", key: "CORE-3", title: "Third issue" },
    issue_key: "CORE-3",
    question: "Third question?",
  });
  let inboxRows = [askA, askB, askC];
  let repliesB: Comment[] = [];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => inboxRows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: [askA, askB, askC].find((ask) => ask.id === id) ?? askA,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: id === askB.id ? repliesB : [],
  }));
  // The server records the clarification: the turn is the agent's, and the inbox says so on
  // the next refetch.
  const createComment = spyOn(api, "createComment").mockImplementation(async (issueKey, input) => {
    const reply: Comment = {
      anchor: null,
      ask_id: input.ask_id ?? null,
      author: { id: "alice", kind: "user" },
      body: input.body,
      created_at: "2026-09-11T02:00:00Z",
      edited_at: null,
      id: "reply-1",
      issue_key: issueKey,
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: "agent",
      ...commentDeliveryFields(),
    };
    repliesB = [reply];
    inboxRows = [
      askA,
      {
        ...askB,
        last_reply: { author: reply.author, created_at: reply.created_at },
        waiting_on: "agent",
      },
      askC,
    ];
    return reply;
  });
  const geometry = stackRows();
  const { unmount } = renderInbox();

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    fireEvent.pointerOver(row);
    fireEvent.change(within(card).getByLabelText("Your answer"), {
      target: { value: "Ship what, exactly?" },
    });
    const askBack = within(card).getByRole("button", { name: "Ask back" });
    askBack.focus();
    await act(async () => {
      fireEvent.click(askBack);
    });

    // The reply lands in the card's thread and the card says whose turn it is; Ask back, its
    // text sent, disables itself and hands focus to the row. The row does not leave from under
    // the reader's hand, and the view is not dragged after it.
    await within(card).findByText("Ship what, exactly?");
    await waitFor(() =>
      expect(screen.getByTestId("turn-ask-b").textContent).toBe("Waiting on session:session-…")
    );
    expect(createComment).toHaveBeenCalledWith(
      "CORE-2",
      {
        ask_id: "ask-b",
        body: "Ship what, exactly?",
      },
      keyedAttempt
    );
    expect(document.activeElement).toBe(row);
    expect(row.getAttribute("data-inbox-section")).toBe("human");
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);
    expect(headings()).toEqual(["Waiting on you"]);
    expect(geometry.scrollBy).not.toHaveBeenCalled();

    // Pointer and focus leave the row (here, k moving focus to the row above): it takes its place
    // under Waiting on agents; the row above did not move, so the view stays.
    fireEvent.pointerOut(row, { relatedTarget: document.body });
    fireEvent.pointerLeave(row);
    const rowAbove = document.querySelector<HTMLElement>('[data-inbox-row="ask-a"]');
    if (rowAbove === null) throw new Error("ask-a row missing");
    act(() => rowAbove.focus());
    await waitFor(() => expect(row.getAttribute("data-inbox-section")).toBe("agent"));
    expect(rowIds()).toEqual(["ask-a", "ask-c", "ask-b"]);
    expect(headings()).toEqual(["Waiting on you", "Waiting on agents"]);
    expect(geometry.scrollBy).not.toHaveBeenCalled();
  } finally {
    unmount();
    geometry.restore();
    createComment.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an ask answered elsewhere stays in place with its recorded answer while the reader is in its row, then leaves", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const answeredB: InboxRow = {
    ...askB,
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "JSON, always.", user: "bob" },
    state: "answered",
  };
  let threadB = askB;
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : threadB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const field = within(card).getByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "Half-typed" } });
    field.focus();
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");

    // Bob answers it in his own tab: the server drops it from the inbox and the thread records
    // the answer.
    threadB = answeredB;
    await act(async () => {
      queryClient.setQueryData<InboxRow[]>(["inbox"], [askA]);
      await queryClient.invalidateQueries({ queryKey: ["ask-thread", "ask-b"] });
    });

    const kept = await screen.findByTestId("ask-ask-b");
    expect(kept.closest("[data-inbox-row]")).toBe(row);
    expect(within(kept).getByText("bob")).toBeTruthy();
    expect(within(kept).getByText("JSON, always.")).toBeTruthy();
    // The answer form is gone; the row keeps keyboard focus so j/k/Escape still start here.
    expect(document.activeElement).toBe(row);

    // Moving on (here, j/k moving focus to the next row) releases it.
    const otherRow = document.querySelector<HTMLElement>('[data-inbox-row="ask-a"]');
    if (otherRow === null) throw new Error("ask-a row missing");
    act(() => otherRow.focus());
    await waitFor(() => expect(screen.queryByTestId("ask-ask-b")).toBeNull());
    expect(screen.getByTestId("ask-ask-a")).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the reader's own answer leaves the Inbox at once, even though their focus and pointer were on the row", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which format?",
  });
  let inboxRows = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => inboxRows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    inboxRows = [askA];
    return {
      ...askB,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    fireEvent.pointerOver(row);
    fireEvent.click(await within(card).findByRole("radio", { name: "Ship" }));
    const answer = within(card).getByRole("button", { name: "Answer" });
    answer.focus();
    await act(async () => {
      fireEvent.click(answer);
    });
    answer.focus();

    await waitFor(() => expect(screen.queryByTestId("ask-ask-b")).toBeNull());
    expect(screen.getByTestId("ask-ask-a")).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("after the reader's own answer fails, an answer from elsewhere still holds the row they are in", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  let threadB = askB;
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : threadB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(new Error("offline"));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    const field = within(card).getByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "JSON" } });
    const answer = within(card).getByRole("button", { name: "Answer" });
    answer.focus();
    await act(async () => {
      fireEvent.click(answer);
    });
    // The optimistic removal does not take the row from under the reader: it stays, and the
    // failure shows on it.
    await waitFor(() => expect(within(card).getByRole("alert")).toBeTruthy());
    expect(answerAsk).toHaveBeenCalledTimes(1);
    expect(within(card).getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe("JSON");
    field.focus();

    // Bob answers it meanwhile: the reader is still in the row, so it stays.
    threadB = {
      ...askB,
      answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "YAML.", user: "bob" },
      state: "answered",
    };
    await act(async () => {
      queryClient.setQueryData<InboxRow[]>(["inbox"], [askA]);
      await queryClient.invalidateQueries({ queryKey: ["ask-thread", "ask-b"] });
    });

    const kept = await screen.findByTestId("ask-ask-b");
    expect(kept.closest("[data-inbox-row]")).toBe(row);
    expect(within(kept).getByText("YAML.")).toBeTruthy();
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an ask card offers backlinks only when the batched count says it has some", async () => {
  const cited = issueAsk({ id: "ask-cited", referenced_by_count: 2 });
  const uncited = issueAsk({ id: "ask-uncited", referenced_by_count: 0 });
  const rows = [cited, uncited];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();

  try {
    const citedCard = await screen.findByTestId("ask-ask-cited");
    const uncitedCard = screen.getByTestId("ask-ask-uncited");
    expect(within(citedCard).getByRole("button", { name: "Referenced by (2)" })).toBeTruthy();
    expect(within(uncitedCard).queryByText(/Referenced by/)).toBeNull();
    // The count came with the list: no card fetches the graph until the reader opens one.
    await waitFor(() => expect(mocks.getReferences).toHaveBeenCalledTimes(0));

    fireEvent.click(within(citedCard).getByRole("button", { name: "Referenced by (2)" }));
    await waitFor(() => expect(mocks.getReferences).toHaveBeenCalledTimes(1));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an unavailable backlink list stays quiet beside an ask card", async () => {
  const row = issueAsk({ id: "ask-backlink", referenced_by_count: 1 });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([row]);
  const getAsk = mockAskReads([row]);
  mocks.getReferences.mockRejectedValue(new Error("offline"));
  const { unmount } = renderInbox();

  try {
    const card = await screen.findByTestId("ask-ask-backlink");
    fireEvent.click(within(card).getByRole("button", { name: "Referenced by (1)" }));
    expect(await within(card).findByText("Referenced by unavailable.")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(within(card).getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** The address the Inbox is on, and a way back through the history it has written. */
function RouteProbe() {
  const location = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <output data-testid="location">{`${location.pathname}${location.search}`}</output>
      <button onClick={() => navigate(-1)} type="button">
        History back
      </button>
    </>
  );
}

/** `route` is the whole history when it is a list, the last entry being where the reader is:
 *  whether a filter change replaces its entry or pushes one is only visible from a history that
 *  has something underneath it. `runtime` is for the tests whose reading column opens a
 *  document: that column mounts `ProofDocument`, which wants the document runtime and the
 *  margin the app shell gives it, and every other test's column shows a card and needs
 *  neither. */
function renderInbox(
  route: string | readonly string[] = "/",
  { runtime }: { runtime?: DocumentRuntimeValue } = {}
) {
  const entries = typeof route === "string" ? [route] : [...route];
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const inbox = (
    <KeymapProvider>
      <RouteProbe />
      <Inbox />
    </KeymapProvider>
  );
  const view = render(
    <MemoryRouter initialEntries={entries} initialIndex={entries.length - 1}>
      <QueryClientProvider client={queryClient}>
        {runtime === undefined ? (
          inbox
        ) : (
          <DocumentRuntime.Provider value={runtime}>
            <MarginProvider>{inbox}</MarginProvider>
          </DocumentRuntime.Provider>
        )}
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

/** Who the filter's popover lists: `GET /users`, read on its first open. */
function mockPeople() {
  return spyOn(api, "listUsers").mockResolvedValue([{ login: "alice" }, { login: "bob" }]);
}

function filterTrigger(): HTMLElement {
  return screen.getByRole("button", { name: "Filter by assignee" });
}

/** Opens the assignee filter if it is shut and toggles one of its rows. The popover is portalled
 *  onto the body, so its options are reached from the screen rather than from the list. */
async function toggleAssignee(option: RegExp) {
  if (filterTrigger().getAttribute("aria-expanded") !== "true") fireEvent.click(filterTrigger());
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

function headings(): string[] {
  return screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent ?? "");
}

/** The rows' ask ids in DOM order: the order the reader sees. */
function rowIds(): string[] {
  return [...document.querySelectorAll("[data-inbox-row]")].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

const ROW_HEIGHT = 200;

/** A fixed geometry happy-dom lacks: rows stacked in DOM order, ROW_HEIGHT each, so a row that
 *  changes place in the list changes its measured top and the ViewportAnchor would scroll after
 *  it; `scrollBy` records whether it did. */
function stackRows() {
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const all = [...document.querySelectorAll("[data-inbox-row]")];
    const index = all.indexOf(this);
    const top = index === -1 ? 0 : index * ROW_HEIGHT;
    const height = index === -1 ? all.length * ROW_HEIGHT : ROW_HEIGHT;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 800,
      toJSON: () => ({}),
      top,
      width: 800,
      x: 0,
      y: top,
    };
  });
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  return {
    restore: () => {
      rect.mockRestore();
      scrollBy.mockRestore();
    },
    scrollBy,
  };
}

/** One listed row by its ask id, for the controls that sit on it. */
function rowOf(askId: string): HTMLElement {
  const row = screen.getByTestId(`ask-${askId}`).closest<HTMLElement>("[data-inbox-row]");
  if (row === null) throw new Error(`${askId} row missing`);
  return row;
}

/** The reader, the other person, the issue nobody holds, and a document question - every side
 *  of the assignee filter in one list. */
function filterRows(): InboxRow[] {
  return [
    issueAsk({ id: "ask-mine" }),
    issueAsk({
      id: "ask-bob",
      issue: { assignee: "bob", key: "CORE-2", title: "Bob's issue" },
      issue_key: "CORE-2",
      question: "Bob's question?",
    }),
    issueAsk({
      id: "ask-orphan",
      issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
      issue_key: "CORE-3",
      question: "Orphan question?",
    }),
    artifactAsk(),
  ];
}

test("an address without ?assignee= lists the reader's own questions alone, under no view switch", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which approach?");
    expect(rowIds()).toEqual(["ask-mine"]);
    expect(headings()).toEqual(["Waiting on you"]);
    expect(filterTrigger().textContent).toBe("Assignee · you");
    // The Mine/Everyone switch is gone; the filter is the only way to change who the list is for.
    expect(screen.queryByRole("button", { name: "Mine" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Everyone" })).toBeNull();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("picking another person lists their questions at ?assignee=<email>, under Waiting on a person", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which approach?");
    // The reader's own list is the one band that can say "you".
    expect(headings()).toEqual(["Waiting on you"]);

    await toggleAssignee(/bob/);
    await toggleAssignee(/alice/);

    await screen.findByText("Bob's question?");
    expect(screen.getByTestId("location").textContent).toBe("/?assignee=bob");
    expect(rowIds()).toEqual(["ask-bob"]);
    expect(headings()).toEqual(["Waiting on a person"]);
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("?assignee=everyone lists every open question, the document ones included", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Bob's question?");
    expect(rowIds()).toEqual(["ask-mine", "ask-bob", "ask-orphan", "ask-1"]);
    expect(filterTrigger().textContent).toBe("Assignee · everyone");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("?assignee=unassigned lists what nobody holds, with Assign to me on the issue rows only", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?assignee=unassigned");
  try {
    await screen.findByText("Orphan question?");
    expect(rowIds()).toEqual(["ask-orphan", "ask-1"]);
    expect(filterTrigger().textContent).toBe("Assignee · Unassigned");
    expect(
      within(rowOf("ask-orphan")).getByRole("button", { name: "Assign CORE-3 to me" })
    ).toBeTruthy();
    // A document ask has no issue to take.
    expect(within(rowOf("ask-1")).queryByRole("button", { name: /Assign .* to me/ })).toBeNull();
    expect(screen.getAllByRole("button", { name: /Assign .* to me/ })).toHaveLength(1);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an address naming somebody who has never signed in lists nothing and refuses nothing", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?assignee=zoe@example.com");
  try {
    expect(await screen.findByText("Nothing matches this filter")).toBeTruthy();
    expect(rowIds()).toEqual([]);
    expect(screen.queryByRole("alert")).toBeNull();
    // A filter read from the address says what it is filtering by, listed person or not.
    expect(filterTrigger().textContent).toBe("Assignee · zoe@example.com");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an empty list speaks for whoever it was for: nothing needs you, or nothing needs anyone", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(
    rows.filter((row) => row.id !== "ask-mine")
  );
  const getAsk = mockAskReads(rows);
  const own = renderInbox();
  try {
    expect(await screen.findByText("Nothing needs you")).toBeTruthy();
  } finally {
    own.unmount();
  }
  getInbox.mockResolvedValue([]);
  const cleared = renderInbox("/?assignee=everyone");
  try {
    expect(await screen.findByText("Nothing needs anyone")).toBeTruthy();
  } finally {
    cleared.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a filter change replaces the history entry instead of piling one up per pick", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox(["/agents", "/"]);
  try {
    await screen.findByText("Which approach?");
    await toggleAssignee(/bob/);
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/?assignee=alice&assignee=bob")
    );

    fireEvent.click(screen.getByRole("button", { name: "History back" }));
    // Back leaves the Inbox altogether: the pick replaced the entry it was made on.
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/agents"));
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a caller supplying onAssigneesChange (the Inbox drawer) keeps the selection local: the address is untouched", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const changes: (readonly string[] | undefined)[] = [];
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RouteProbe />
        <Inbox
          filter={{}}
          keymapScope={DIALOG_SCOPE}
          onAssigneesChange={(next) => changes.push(next)}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText("Which approach?");
    await toggleAssignee(/bob/);
    expect(changes).toEqual([["alice", "bob"]]);
    expect(screen.getByTestId("location").textContent).toBe("/");
    // The caller owns the filter, so the page it is embedded on lists what it lists.
    expect(screen.queryByText("Bob's question?")).toBeNull();
  } finally {
    view.unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Assign to me PATCHes the viewer's lowercase login and keeps its row until the save lands", async () => {
  const orphan = issueAsk({
    id: "ask-orphan",
    issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
    issue_key: "CORE-3",
    question: "Orphan question?",
  });
  const rows = [orphan];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { unmount } = renderInbox("/?assignee=unassigned");
  try {
    await screen.findByText("Orphan question?");
    fireEvent.click(screen.getByRole("button", { name: "Assign CORE-3 to me" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }, keyedAttempt)
    );
    // The optimistic assignment takes the row out of this filter at once; the live write keeps
    // it on screen, so `Assigning…` is shown by the control the reader clicked.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Assign CORE-3 to me" }).textContent).toBe(
        "Assigning…"
      )
    );
    expect(rowIds()).toEqual(["ask-orphan"]);
    // Once the server has recorded it, the refetched inbox carries the new assignee.
    getInbox.mockResolvedValue([
      { ...orphan, issue: { assignee: "alice", key: "CORE-3", title: "Nobody's issue" } },
    ]);
    await act(async () => {
      save.resolve(narrowIssueOf(orphan, "alice"));
      await save.promise;
    });
    // Nobody's no longer, the row leaves this filter once the save settles and the refetch
    // lands; the box under test can be slow to deliver both, so this wait is generous.
    await waitFor(() => expect(rowIds()).toEqual([]), { timeout: 4000 });
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a refused Assign to me keeps its row under the Unassigned filter, with the reason and a working Retry", async () => {
  const orphan = issueAsk({
    id: "ask-orphan",
    issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
    issue_key: "CORE-3",
    question: "Orphan question?",
  });
  const rows = [orphan];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const firstSave = Promise.withResolvers<Issue>();
  let attempts = 0;
  const patchIssue = answerIssuePatches().mockImplementation(async () => {
    attempts += 1;
    return attempts === 1 ? firstSave.promise : narrowIssueOf(orphan, "alice");
  });
  const { unmount } = renderInbox("/?assignee=unassigned");
  try {
    await screen.findByText("Orphan question?");
    fireEvent.click(screen.getByRole("button", { name: "Assign CORE-3 to me" }));
    // Assigned optimistically, the row matches nothing this filter lists - and stays.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Assign CORE-3 to me" }).textContent).toBe(
        "Assigning…"
      )
    );
    expect(rowIds()).toEqual(["ask-orphan"]);

    await act(async () => {
      firstSave.reject(
        new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-3 is closed; reopen it first" })
      );
      await firstSave.promise.catch(() => undefined);
    });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("CORE-3 is closed; reopen it first");
    expect(rowIds()).toEqual(["ask-orphan"]);

    getInbox.mockResolvedValue([
      { ...orphan, issue: { assignee: "alice", key: "CORE-3", title: "Nobody's issue" } },
    ]);
    fireEvent.click(within(alert).getByRole("button", { name: /Retry/ }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(rowIds()).toEqual([]), { timeout: 4000 });
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a sibling ask of the same issue leaving the list does not drop an in-flight Assign to me: its refusal still shows", async () => {
  const askA = issueAsk({
    id: "ask-a",
    issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
    issue_key: "CORE-3",
    question: "Question A?",
  });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
    issue_key: "CORE-3",
    question: "Question B?",
  });
  const rows = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount } = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Question A?");
    const rowA = () => screen.getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    fireEvent.click(
      within(rowA() as HTMLElement).getByRole("button", { name: "Assign CORE-3 to me" })
    );
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    // Both rows are the viewer's optimistically, so B loses its own control; A's write is live.
    await waitFor(() =>
      expect(
        within(rowA() as HTMLElement).getByRole("button", { name: "Assign CORE-3 to me" })
          .textContent
      ).toBe("Assigning…")
    );
    expect(within(rowOf("ask-b")).queryByRole("button", { name: /Assign .* to me/ })).toBeNull();

    // B is answered elsewhere and leaves the list while A's save is still in flight.
    act(() => {
      queryClient.setQueriesData<InboxRow[]>({ queryKey: ["inbox"] }, (current) =>
        current?.filter((row) => row.id !== "ask-b")
      );
    });
    await waitFor(() => expect(screen.queryByText("Question B?")).toBeNull());
    expect(
      within(rowA() as HTMLElement).getByRole("button", { name: "Assign CORE-3 to me" })
    ).toBeTruthy();

    // The server refuses: A rolls back, and its own control shows the reason.
    await act(async () => {
      save.reject(
        new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-3 is closed; reopen it first" })
      );
      await save.promise.catch(() => undefined);
    });
    const alert = await within(rowA() as HTMLElement).findByRole("alert");
    expect(alert.textContent).toContain("CORE-3 is closed; reopen it first");
    expect(rowIds()).toContain("ask-a");
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Blocked on you counts the reader's own questions, and does not move when the filter widens", async () => {
  const mine = issueAsk({ id: "ask-mine" });
  const bobs = issueAsk({
    id: "ask-bob",
    issue: { assignee: "bob", key: "CORE-2", title: "Bob's issue" },
    issue_key: "CORE-2",
    question: "Bob's question?",
  });
  const orphan = issueAsk({
    id: "ask-orphan",
    issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
    issue_key: "CORE-3",
    question: "Orphan question?",
  });
  const rows = [mine, bobs, orphan];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const own = renderInbox();
  try {
    await screen.findByText("Which approach?");
    expect(rowIds()).toEqual(["ask-mine"]);
    // One question of the three is on the reader's own issue; the banner counts that one.
    expect(screen.getByText(/Blocked on you: 1 item/)).toBeTruthy();
  } finally {
    own.unmount();
  }
  const everyone = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Bob's question?");
    expect(rowIds()).toEqual(["ask-mine", "ask-bob", "ask-orphan"]);
    // The list widened; what waits on the reader did not, so the banner says the same thing.
    expect(screen.getByText(/Blocked on you: 1 item/)).toBeTruthy();
  } finally {
    everyone.unmount();
  }
  const narrowed = renderInbox("/?assignee=unassigned");
  try {
    await screen.findByText("Orphan question?");
    expect(screen.getByText(/Blocked on you: 1 item/)).toBeTruthy();
  } finally {
    narrowed.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** An hour from now, as the server stores it: inside any snooze window the reader can pick. */
function soon(hours = 3): string {
  return new Date(Date.now() + hours * 60 * 60 * 1000).toISOString();
}

test("Snooze folds a row into Later until the reader un-snoozes it", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  // The server's answer moves with the writes, so the refetch each write triggers returns what
  // the server would now hold rather than resurrecting the pre-snooze list.
  let served: InboxRow[] = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const unsnoozeAsk = spyOn(api, "unsnoozeAsk").mockImplementation(async (id) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: null } : row));
  });
  const getAsk = mockAskReads([askA, askB]);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which format?");
    expect(headings()).toEqual(["Waiting on you"]);
    expect(screen.getByText(/Blocked on you: 2 items/)).toBeTruthy();

    const row = screen.getByTestId("ask-ask-b").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    fireEvent.change(within(row).getByLabelText("Snooze CORE-2: Which format?"), {
      target: { value: "tomorrow" },
    });

    // Folded by default: the row leaves the list, and the band's count says where it went.
    await waitFor(() => expect(headings()).toEqual(["Waiting on you", "Later (1)"]));
    expect(rowIds()).toEqual(["ask-a"]);
    expect(screen.getByText(/Blocked on you: 1 item/)).toBeTruthy();
    expect(snoozeAsk).toHaveBeenCalledTimes(1);
    const [, body] = snoozeAsk.mock.calls[0] ?? [];
    expect(Date.parse(body?.snoozed_until ?? "")).toBeGreaterThan(Date.now());

    fireEvent.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    const later = screen.getByTestId("ask-ask-b").closest<HTMLElement>("[data-inbox-row]");
    expect(later?.getAttribute("data-inbox-section")).toBe("later");

    fireEvent.click(
      within(later as HTMLElement).getByRole("button", { name: "Un-snooze CORE-2: Which format?" })
    );
    await waitFor(() => expect(headings()).toEqual(["Waiting on you"]));
    expect(unsnoozeAsk).toHaveBeenCalledWith("ask-b", keyedAttempt);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(screen.getByText(/Blocked on you: 2 items/)).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
    unsnoozeAsk.mockRestore();
  }
});

test("an agent's reply on a snoozed row leaves it in Later and off Waiting on you", async () => {
  const snoozed = issueAsk({
    id: "ask-snoozed",
    question: "Which format?",
    snoozed_until: soon(),
    waiting_on: "agent",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([snoozed]);
  const getAsk = mockAskReads([snoozed]);
  const { queryClient, unmount } = renderInbox();
  try {
    await waitFor(() => expect(headings()).toEqual(["Later (1)"]));
    expect(rowIds()).toEqual([]);

    // The agent answers: the turn is the reader's again and the row carries the agent's reply -
    // exactly the change that could pull a deferred ask back onto the list.
    act(() => {
      queryClient.setQueryData<InboxRow[]>(
        ["inbox"],
        [
          {
            ...snoozed,
            last_reply: {
              author: { id: "session-1", kind: "session" },
              created_at: "2026-09-11T01:00:00Z",
            },
            waiting_on: "human",
          },
        ]
      );
    });

    expect(headings()).toEqual(["Later (1)"]);
    expect(screen.queryByText("Waiting on you")).toBeNull();
    expect(screen.queryByText(/Blocked on you/)).toBeNull();
    expect(rowIds()).toEqual([]);

    // Opening the band shows it there, still deferred, with its return time.
    fireEvent.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    const row = await screen.findByTestId("ask-ask-snoozed");
    expect(row.closest("[data-inbox-section]")?.getAttribute("data-inbox-section")).toBe("later");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a snooze whose moment has passed is an ordinary row again", async () => {
  const lapsed = issueAsk({
    id: "ask-lapsed",
    question: "Which format?",
    snoozed_until: "2020-01-01T00:00:00.000Z",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([lapsed]);
  const getAsk = mockAskReads([lapsed]);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which format?");
    expect(headings()).toEqual(["Waiting on you"]);
    expect(rowIds()).toEqual(["ask-lapsed"]);
    expect(screen.getByText(/Blocked on you: 1 item/)).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a snooze still saving says Snoozing…, not Un-snoozing…, in the band it has moved to", async () => {
  const row = issueAsk({ id: "ask-b", question: "Which format?" });
  let served: InboxRow[] = [row];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  // Held open, so the render under test is the one while the write is in flight.
  let settle: (() => void) | undefined;
  const snoozeAsk = answerAskSnoozes().mockImplementation(
    (id, { snoozed_until: until }) =>
      new Promise((resolve) => {
        settle = () => {
          served = served.map((each) =>
            each.id === id ? { ...each, snoozed_until: until } : each
          );
          resolve({ snoozed_until: until });
        };
      })
  );
  const getAsk = mockAskReads([row]);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which format?");
    fireEvent.change(screen.getByLabelText("Snooze CORE-1: Which format?"), {
      target: { value: "tomorrow" },
    });

    // The optimistic update has already put the row in Later, which is right - it is where the
    // snooze is going - but the label must say what the reader did, not where the row landed.
    await waitFor(() => expect(headings()).toEqual(["Later (1)"]));
    fireEvent.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    expect(screen.getByText("Snoozing…")).toBeTruthy();
    expect(screen.queryByText("Un-snoozing…")).toBeNull();
    // The optimistic return time is shown beside it; that is the snooze being saved.
    expect(document.querySelector("[data-inbox-snoozed-until]")).toBeTruthy();

    // Once it settles the row reports its state again, not the last thing done to it.
    await act(async () => {
      settle?.();
      await Promise.resolve();
    });
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Un-snooze CORE-1: Which format?" }).textContent
      ).toBe("Un-snooze")
    );
    expect(screen.queryByText("Snoozing…")).toBeNull();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("a refused snooze rolls the row back and says why, where the reader can still see it", async () => {
  const row = issueAsk({ id: "ask-b", question: "Which format?" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([row]);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
  );
  const getAsk = mockAskReads([row]);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which format?");
    fireEvent.change(screen.getByLabelText("Snooze CORE-1: Which format?"), {
      target: { value: "tomorrow" },
    });

    // The row comes back to the band it was in, and the reason comes with it - the control is
    // kept mounted through the fold, so the refusal is not thrown away with the row.
    await waitFor(() => expect(headings()).toEqual(["Waiting on you"]));
    expect(rowIds()).toEqual(["ask-b"]);
    expect(screen.getByText("snoozed_until must be in the future")).toBeTruthy();
    expect(screen.getByText(/Blocked on you: 1 item/)).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// ----------------------------------------------------------------------------------------
// The reading column (INFRA-575). Every test above this line runs at happy-dom's own 1024 px,
// where the Inbox is the list of cards it has always been - the Inbox below 1280 px, which the
// spec keeps unchanged - so the tests below say where they stand by widening the window first.
// ----------------------------------------------------------------------------------------

/** The width the Inbox shows its reading column from (`READING_PANE_QUERY`, 1280 px). happy-dom
 *  answers `matchMedia` from the window it is given, so the window is the whole fixture. */
function widenToReadingPane(): () => void {
  const window_ = window as unknown as {
    happyDOM: { setViewport(size: { width: number }): void };
    innerWidth: number;
  };
  const was = window_.innerWidth;
  window_.happyDOM.setViewport({ width: 1440 });
  return () => window_.happyDOM.setViewport({ width: was });
}

/** One row by its ask id: a short row holds no card, so there is no `ask-<id>` to find it by. */
function shortRow(askId: string): HTMLElement {
  const row = document.querySelector<HTMLElement>(`[data-inbox-row="${askId}"]`);
  if (row === null) throw new Error(`${askId} row missing`);
  return row;
}

/** The ask ids of the rows marked selected: exactly one, or the test has found a second. */
function selectedIds(): string[] {
  return [...document.querySelectorAll('[data-inbox-selected="true"]')].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

function address(): string {
  return screen.getByTestId("location").textContent ?? "";
}

/** The pane reads one field of the question's issue: whether it is closed. */
function mockIssueReads() {
  return spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails);
}

test("from 1280 px the Inbox is two columns: short rows in the list, the question in the pane", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "# Which approach?\n\nThe detail.");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const rows = [first, second];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const list = await screen.findByTestId("inbox-list");
    const pane = await screen.findByTestId("inbox-pane");
    // The pane is the column to the right of the list: one row, the list first.
    expect(list.parentElement).toBe(pane.parentElement?.parentElement ?? null);
    expect(list.compareDocumentPosition(pane) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // The card is the pane's, never the row's; the row is the question's first line, its kind
    // and its age.
    expect(within(list).queryByTestId("ask-ask-a")).toBeNull();
    expect(within(pane).getByTestId("ask-ask-a")).toBeTruthy();
    const row = shortRow("ask-a");
    expect(within(row).getByText("Which approach?")).toBeTruthy();
    expect(within(row).getByText("Question")).toBeTruthy();
    expect(within(row).getByText("CORE-1")).toBeTruthy();
    expect(row.querySelector("time")?.getAttribute("datetime")).toBe(first.created_at);
    // The first row is selected when the Inbox opens, and the address says so.
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(address()).toBe("/?item=ask-a");
    expect(shortRow("ask-b").getAttribute("data-inbox-selected")).toBeNull();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px the Inbox is today's Inbox: the card in the row, no pane and no ?item=", async () => {
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();

  try {
    const card = await screen.findByTestId("ask-ask-a");
    expect(card.closest("[data-inbox-row]")).toBe(shortRow("ask-a"));
    expect(screen.queryByTestId("inbox-pane")).toBeNull();
    expect(selectedIds()).toEqual([]);
    expect(address()).toBe("/");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a short row names its kind: a decision block, an approval request, and any other question", async () => {
  const narrow = widenToReadingPane();
  const question = inboxIssue("ask-question", "CORE-1", "Which approach?");
  const decision = inboxIssue("ask-decision", "CORE-2", "Ship behind a flag?", {
    block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
    block_id: "ship_it",
  });
  const approval = inboxIssue("ask-approval", "CORE-3", "Approve spec.md (version 4)?", {
    approval: { artifact_id: "artifact-1", name: "spec.md", requested_version: 4, version: 4 },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    kind: "approval",
  });
  const rows = [question, decision, approval];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    expect(within(shortRow("ask-question")).getByText("Question")).toBeTruthy();
    expect(within(shortRow("ask-decision")).getByText("Decision")).toBeTruthy();
    expect(within(shortRow("ask-approval")).getByText("Approval")).toBeTruthy();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("?item= selects the row it names, and one the list does not hold falls back to the first", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const rows = [first, second];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const named = renderInbox("/?item=ask-b");

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
  } finally {
    named.unmount();
  }

  const gone = renderInbox("/?item=ask-answered-elsewhere");
  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    // The address is rewritten to what the Inbox actually shows, so a reload lands there.
    expect(address()).toBe("/?item=ask-a");
  } finally {
    gone.unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("?item= naming a row in the folded Later band opens the band and selects it", async () => {
  const narrow = widenToReadingPane();
  const open = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const later = inboxIssue("ask-later", "CORE-2", "Which format?", { snoozed_until: soon() });
  const rows = [open, later];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?item=ask-later");

  try {
    await screen.findByTestId("inbox-pane");
    // A folded band renders no rows at all, so the row being there is the band being open.
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-later"]));
    expect(selectedIds()).toEqual(["ask-later"]);
    expect(screen.getByRole("button", { name: /^Later \(1\)/ }).getAttribute("aria-expanded")).toBe(
      "true"
    );
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("j and k move the selection down and up the list, the address following each step", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    fireEvent.keyDown(document.body, { key: "j" });
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    // The selection is state, not focus: nothing in the list took the keyboard.
    expect(document.activeElement).toBe(document.body);

    fireEvent.keyDown(document.body, { key: "k" });
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    expect(address()).toBe("/?item=ask-a");
    // The last row has nothing below it, and the first nothing above it.
    fireEvent.keyDown(document.body, { key: "k" });
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a click on a row selects it", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    fireEvent.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The Inbox on a route of its own, as the app mounts it (`app.tsx`): a link out of the list
 *  unmounts it, so the address a followed link leaves behind is the link's. */
function renderRoutedInbox() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RouteProbe />
          <Routes>
            <Route element={<Inbox />} path="/" />
            <Route element={<p>The ask in its issue</p>} path="/issues/:key/asks/:id" />
          </Routes>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { unmount: view.unmount };
}

test("a click on a short row's owner link opens the ask inside its issue, and the row leaves that address alone", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderRoutedInbox();

  try {
    await screen.findByTestId("inbox-pane");
    // The question's own words are the row's: clicking them selects it, so the link followed
    // below is on a row the column is not already showing.
    fireEvent.click(within(shortRow("ask-b")).getByText("Which format?"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");

    // The owner link opens the ask inside its issue, and the row holding it does not replace
    // that address with the Inbox's own.
    const owner = within(shortRow("ask-a")).getByRole("link", { name: /CORE-1/ });
    expect(owner.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-a");
    fireEvent.click(owner);
    expect(address()).toBe("/issues/CORE-1/asks/ask-a");
    expect(screen.queryByTestId("inbox-list")).toBeNull();
    expect(screen.getByText("The ask in its issue")).toBeTruthy();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a short row's checkbox marks the row without selecting it or writing the address", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));

    fireEvent.click(within(shortRow("ask-b")).getByRole("checkbox"));
    await screen.findByText("1 selected");
    expect((within(shortRow("ask-b")).getByRole("checkbox") as HTMLInputElement).checked).toBe(
      true
    );
    // The mark is the checkbox's whole doing: the column is still on the row it was showing.
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(address()).toBe("/?item=ask-a");
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("the reader's own answer takes its row off the list and selects the next one", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = [second];
    return {
      ...first,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    fireEvent.click(within(pane).getByRole("radio", { name: "Ship" }));
    fireEvent.click(within(pane).getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    expect(selectedIds()).toEqual(["ask-b"]);
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The reader picks an option and presses Answer, and nothing of theirs is typed into the box:
// the draft that would otherwise keep the question in the column is empty, so this reads the
// hold the answer itself takes. Reverted, the optimistic removal hands the selection on at once
// and unmounts the card, which is the only surface the refusal is shown on.
test("an answer the server refuses keeps its question in the column, and a retry that lands moves on", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const refusal = Promise.withResolvers<never>();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(() => refusal.promise);
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    fireEvent.click(within(pane).getByRole("radio", { name: "Ship" }));
    fireEvent.click(within(pane).getByRole("button", { name: "Answer" }));

    // The optimistic removal takes the row at once; the column keeps the question, because the
    // answer is still the reader's to finish, and the selection stays on it.
    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-a");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-a")).toBeTruthy();
    expect(within(screen.getByTestId("inbox-pane")).getByText("Answering…")).toBeTruthy();

    await act(async () => {
      refusal.reject(
        new ApiError(400, {
          code: "ASK_REVISION_REQUIRED",
          error: "expected_edited_at is required",
        })
      );
      await Promise.resolve();
    });

    // The reason and its Retry are in the card that wrote, under what the reader answered, and
    // the rolled-back row is listed again with the selection still on it.
    const refused = await waitFor(() =>
      within(screen.getByTestId("inbox-pane")).getByRole("alert")
    );
    expect(refused.textContent).toContain("Could not save your answer.");
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(address()).toBe("/?item=ask-a");

    // The server records the same answer on the card's own Retry: that is when the row leaves
    // and the next visible row in list order is selected.
    answerAsk.mockImplementation(async () => {
      served = [second];
      return {
        ...first,
        answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
        state: "answered",
      };
    });
    fireEvent.click(
      within(screen.getByTestId("inbox-pane")).getByRole("button", { name: "Retry" })
    );

    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(rowIds()).toEqual(["ask-b"]);
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a selected row the server drops leaves the list at once and the next row is selected", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const third = inboxIssue("ask-c", "CORE-3", "Which order?");
  let served = [first, second, third];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(served);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    // The middle row, so the next row in list order and the first row are not the same answer.
    fireEvent.keyDown(document.body, { key: "j" });
    expect(selectedIds()).toEqual(["ask-b"]);

    // Someone else answers the selected question while the reader is reading it. They have
    // typed nothing into it, so nothing holds it: a withdrawn question with a live answer form
    // invites an answer the server refuses.
    served = [first, third];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    expect(rowIds()).toEqual(["ask-a", "ask-c"]);

    // The selection goes where that row was heading - the next row the reader can see, not the
    // first one - the address follows it, and the column shows that row's question.
    expect(selectedIds()).toEqual(["ask-c"]);
    expect(address()).toBe("/?item=ask-c");
    const pane = screen.getByTestId("inbox-pane");
    expect(within(pane).getByTestId("ask-ask-c")).toBeTruthy();
    expect(within(pane).queryByTestId("ask-ask-b")).toBeNull();

    // An empty list shows no row at all, and no column beside it. The address is left as it
    // was: a list holding nothing answers no `?item=`, since it cannot tell a list that is
    // empty from one still loading or failed.
    served = [];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual([]));
    expect(selectedIds()).toEqual([]);
    expect(screen.queryByTestId("inbox-pane")).toBeNull();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The ask's thread read once an agent has retracted it: what the card draws its closed state
 *  from, whatever the Inbox list still says. */
function retracted(row: InboxRow): AskRead {
  return {
    answers: [],
    ask: {
      ...row,
      resolution: {
        actor: { id: "session-1", kind: "session" },
        at: "2026-09-11T03:00:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
      state: "resolved",
    },
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
  };
}

test("a question the reader was answering keeps its text in the column, closed, until they move", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const typed = "Only on a cold start, and only back to the last lease.";
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    fireEvent.change(within(pane).getByLabelText("Your answer"), { target: { value: typed } });

    // The server withdraws the question the reader is answering: it leaves the list like any
    // other dropped row, and its own read says it is closed.
    served = [second];
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-a"], retracted(first));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    expect(rowIds()).toEqual(["ask-b"]);

    // The column keeps the question, shows its closed state in place of the answer form, and
    // keeps what the reader typed where they can read and copy it.
    const kept = within(screen.getByTestId("inbox-pane"));
    expect(kept.getByTestId("ask-resolution-badge").textContent).toBe("Retracted");
    expect(kept.queryByRole("button", { name: "Answer" })).toBeNull();
    expect(document.querySelector("[data-ask-answer]")).toBeNull();
    expect(kept.getByTestId("ask-unsent-answer").textContent).toContain(typed);

    // Until the reader selects another row, which shows that row's question and nothing of
    // this one.
    fireEvent.keyDown(document.body, { key: "j" });
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    const after = within(screen.getByTestId("inbox-pane"));
    expect(after.getByTestId("ask-ask-b")).toBeTruthy();
    expect(after.queryByTestId("ask-ask-a")).toBeNull();
    expect(after.queryByTestId("ask-unsent-answer")).toBeNull();

    // The same holds when the row that goes is the last one: an empty list shows no row, and
    // the column is still the reader's text and the question it belongs to.
    const lastTyped = "Two attempts, then the mirror takes over.";
    fireEvent.change(after.getByLabelText("Your answer"), { target: { value: lastTyped } });
    served = [];
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-b"], retracted(second));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual([]));
    const last = within(screen.getByTestId("inbox-pane"));
    expect(last.getByTestId("ask-resolution-badge").textContent).toBe("Retracted");
    expect(last.getByTestId("ask-unsent-answer").textContent).toContain(lastTyped);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** Where a question the column holds past its row stood in the list, and where each key goes
 *  from there: the step starts from the place that row held, so `k` never skips the row above
 *  it and `j` never doubles back up the list. A step off either end of the list moves nothing,
 *  exactly as it moves nothing from the row at that end. */
const heldSteps = [
  ["j", "the last row", 2, []],
  ["k", "the last row", 2, ["ask-b"]],
  ["j", "a middle row", 1, ["ask-c"]],
  ["k", "a middle row", 1, ["ask-a"]],
  ["j", "the first row", 0, ["ask-b"]],
  ["k", "the first row", 0, []],
] as const;

test.each(
  heldSteps
)("%s from a question held past %s steps from where that row stood", async (key, _place, heldIndex, expected) => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  const held = rows[heldIndex];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    for (let above = 0; above < heldIndex; above += 1) {
      fireEvent.keyDown(document.body, { key: "j" });
      await waitFor(() => expect(selectedIds()).toEqual([rows[above + 1].id]));
    }
    await waitFor(() => expect(selectedIds()).toEqual([held.id]));
    // Typed and unsent: the column keeps this question once its row goes.
    fireEvent.change(within(screen.getByTestId("inbox-pane")).getByLabelText("Your answer"), {
      target: { value: "Whichever the mirror already holds." },
    });

    served = rows.filter((row) => row.id !== held.id);
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", held.id], retracted(held));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual(served.map((row) => row.id)));
    expect(selectedIds()).toEqual([]);

    fireEvent.keyDown(document.body, { key });
    // Settled first, so a row the step should have left alone has its chance to show up.
    await act(async () => {
      await Promise.resolve();
    });
    expect(selectedIds()).toEqual([...expected]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("the keys act on the selected row and the pane: Enter, 1-9, x, h and Escape", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const rows = [first, inboxIssue("ask-b", "CORE-2", "Which format?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");

    // `1` picks the first option of the selected question, inside the pane.
    fireEvent.keyDown(document.body, { key: "1" });
    await waitFor(() =>
      expect((within(pane).getByRole("radio", { name: "Ship" }) as HTMLInputElement).checked).toBe(
        true
      )
    );

    // Enter moves the focus to the pane's answer box, and Escape brings it back to the row.
    fireEvent.keyDown(document.body, { key: "Enter" });
    const answerBox = pane.querySelector("[data-ask-answer]");
    expect(document.activeElement).toBe(answerBox);
    fireEvent.keyDown(answerBox ?? document.body, { key: "Escape" });
    expect(document.activeElement).toBe(shortRow("ask-a"));

    // `h` with nothing checked is the pane head's Snooze, which the short row no longer carries.
    fireEvent.keyDown(document.body, { key: "h" });
    expect(document.activeElement?.matches("[data-inbox-snooze]")).toBe(true);
    expect(pane.contains(document.activeElement)).toBe(true);

    // `x` checks the selected row, which is what the bulk bar then counts.
    fireEvent.keyDown(document.body, { key: "x" });
    await screen.findByText("1 selected");
    expect((within(shortRow("ask-a")).getByRole("checkbox") as HTMLInputElement).checked).toBe(
      true
    );
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("Enter pressed inside the column belongs to the control holding it, and still leaves the list for the answer box", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?", {
      options: [{ label: "Ship" }, { label: "Hold" }],
    }),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    // A control inside the card, holding the focus the card gave it: Enter is that control's,
    // and the Inbox's own binding - which would move the focus to the answer box and swallow
    // the key with `preventDefault` - stands down.
    const option = within(pane).getByRole("radio", { name: "Ship" });
    option.focus();
    fireEvent.keyDown(option, { key: "Enter" });
    expect(document.activeElement).toBe(option);

    // From the list it does its own job: the row hands the focus to the column's answer box.
    shortRow("ask-a").focus();
    fireEvent.keyDown(shortRow("ask-a"), { key: "Enter" });
    expect(document.activeElement).toBe(pane.querySelector("[data-ask-answer]"));
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("with the column on, Enter is the link's or the button's wherever the reader holds it", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    // A link in another row: the owner link, which opens that ask inside its issue. The
    // registry's handler calls `preventDefault` for every binding that fires, so a binding that
    // claimed Enter here would press the link for nothing and pull the focus into the selected
    // question's answer box instead.
    const link = shortRow("ask-b").querySelector<HTMLElement>("[data-inbox-owner]");
    if (link === null) throw new Error("the ask-b row has no owner link");
    link.focus();
    expect(fireEvent.keyDown(link, { key: "Enter" })).toBe(true);
    expect(document.activeElement).toBe(link);
    expect(pane.contains(document.activeElement)).toBe(false);

    // A button outside the list, with the column still showing an answerable question.
    const filter = filterTrigger();
    filter.focus();
    expect(fireEvent.keyDown(filter, { key: "Enter" })).toBe(true);
    expect(document.activeElement).toBe(filter);

    // And the row's own Enter still carries the reader into the column's answer box.
    shortRow("ask-a").focus();
    fireEvent.keyDown(shortRow("ask-a"), { key: "Enter" });
    expect(document.activeElement).toBe(pane.querySelector("[data-ask-answer]"));
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The schema a decision block is built with, so a test can construct the node view the real
 *  editor constructs for every `ask` node of the document (`ProofDocument.decisions.test.tsx`
 *  does the same against the same fake editor). */
const askBlockSchema = new Schema({
  nodes: {
    ask: {
      attrs: {
        answer: { default: undefined },
        answered_at: { default: undefined },
        answered_by: { default: undefined },
        blockId: { default: null },
        invalid: { default: undefined },
        multiple: { default: false },
        selected: { default: undefined },
        state: { default: "open" },
        urgency: { default: "med" },
      },
      content: "paragraph+",
      group: "block",
    },
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
  },
});

function askBlockNode(blockId: string, question: string): ProseMirrorNode {
  return askBlockSchema.node("ask", { blockId }, [
    askBlockSchema.node("paragraph", undefined, [askBlockSchema.text(question)]),
  ]);
}

const SPEC_DOCUMENT: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  slug: "spec",
  versions: [],
};

test("the answer keys act on the selected decision block, not the first one in the document", async () => {
  // Both questions are decision blocks of one document, so the column draws that document and
  // hosts an answerable card for each of them. The selected one is the second card in the DOM.
  const narrow = widenToReadingPane();
  const blockArtifact = { id: "artifact-1", primary: true, slug: "spec" };
  const first = inboxIssue("ask-a", "CORE-1", "Ship behind a flag?", {
    block_artifact: blockArtifact,
    block_id: "block-a",
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-1", "Rename the flag?", {
    block_artifact: blockArtifact,
    block_id: "block-b",
    options: [{ label: "Rename" }, { label: "Keep" }],
  });
  const rows = [first, second];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(SPEC_DOCUMENT);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "",
    version: null,
  });
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue(
    rows.map((row) => ({ ...row, last_reply: null }))
  );
  const { unmount } = renderInbox("/?item=ask-b", { runtime: fake.runtime });

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-b"]);
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the column mounted no editor");
    // The fake editor records the node views the document installs; constructing the `ask` one
    // per block is what the real editor does for the document's two open decisions.
    const nodeViews = editor.viewProps.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    act(() => {
      editor.root.append(construct(askBlockNode("block-a", first.question)).dom);
      editor.root.append(construct(askBlockNode("block-b", second.question)).dom);
    });
    const selectedCard = await within(pane).findByTestId("ask-ask-b");
    const firstCard = within(pane).getByTestId("ask-ask-a");

    // `1` is the selected question's first option, not the first block's.
    fireEvent.keyDown(document.body, { key: "1" });
    await waitFor(() =>
      expect(
        (within(selectedCard).getByRole("radio", { name: "Rename" }) as HTMLInputElement).checked
      ).toBe(true)
    );
    expect(
      (within(firstCard).getByRole("radio", { name: "Ship" }) as HTMLInputElement).checked
    ).toBe(false);

    // One Enter opens the selected card's own note disclosure - the first block's stays shut -
    // and leaves the reader in the box it reveals, because the card focuses the field its own
    // render draws (`AskCard.tsx`). `fireEvent` flushes that render inside the press, as a
    // browser does not, so this row cannot prove the browser case: the proof is the e2e row
    // `Enter opens the selected decision block's note field and puts the focus in it`.
    fireEvent.keyDown(document.body, { key: "Enter" });
    const answerBox = selectedCard.querySelector("[data-ask-answer]");
    expect(answerBox).not.toBeNull();
    expect(firstCard.querySelector("[data-ask-answer]")).toBeNull();
    expect(document.activeElement).toBe(answerBox);
  } finally {
    unmount();
    narrow();
    getArtifact.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifactText.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    listIssueAsks.mockRestore();
  }
});

test("at the column's width with no column on the page, Enter is the focused control's", async () => {
  // The Inbox is wide enough for the reading column and has nothing to select, so no column is
  // mounted: `inbox-pane` is absent, exactly as it is in the commit a snooze of the last
  // selected row leaves behind. The column's keys have nothing to act on there, and the
  // registry's handler calls `preventDefault` for every binding it matches, so a key they claim
  // anyway is a key the control holding the focus never gets - Enter on the assignee filter,
  // on `Answers`, on the `Later` disclosure.
  const narrow = widenToReadingPane();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-list");
    expect(screen.queryByTestId("inbox-pane")).toBeNull();
    const filter = filterTrigger();
    filter.focus();
    // `fireEvent` answers whether the event survived its listeners: the button's own Enter is
    // the browser's default action, so a swallowed key never presses it.
    expect(fireEvent.keyDown(filter, { key: "Enter" })).toBe(true);
    expect(document.activeElement).toBe(filter);
  } finally {
    unmount();
    narrow();
    getInbox.mockRestore();
  }
});

test("a bulk snooze of the selected row keeps the list and the picker the reader is holding", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  // Held open, so the renders under test are the ones the pick leaves behind.
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    // `x` marks the selected row, `j` moves on, `x` marks that one: the whole list.
    fireEvent.keyDown(document.body, { key: "x" });
    fireEvent.keyDown(document.body, { key: "j" });
    fireEvent.keyDown(document.body, { key: "x" });
    await screen.findByText("2 selected");
    fireEvent.keyDown(document.body, { key: "h" });
    const list = screen.getByTestId("inbox-list");
    const picker = screen.getByLabelText("Snooze selected asks");
    expect(document.activeElement).toBe(picker);

    fireEvent.change(picker, { target: { value: "tomorrow" } });
    // Every marked row folds into Later at once, so the selection - the row the column was
    // showing among them - has nothing to fall to and the column goes.
    await waitFor(() => expect(rowIds()).toEqual([]));
    expect(screen.queryByTestId("inbox-pane")).toBeNull();
    // The list and the picker are the same nodes: the column left, the list stayed, and the
    // keyboard is still in the bar it was in.
    expect(screen.getByTestId("inbox-list")).toBe(list);
    expect(screen.getByLabelText("Snooze selected asks")).toBe(picker);
    expect(document.activeElement).toBe(picker);

    await act(async () => {
      landing.resolve();
      await Promise.resolve();
    });
    await waitFor(() => expect(screen.queryByText("Snoozing 2…")).toBeNull());
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("a snooze the column's head writes keeps that column on its row, in flight and refused", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const refusal = Promise.withResolvers<never>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => refusal.promise);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    fireEvent.change(within(pane).getByLabelText("Snooze CORE-1: Which approach?"), {
      target: { value: "tomorrow" },
    });

    // The optimistic snooze has folded the row into Later, and the row is listed inside that
    // fold while its write is out - which is what keeps the column, and the control that
    // wrote, on the page.
    await waitFor(() => expect(headings()).toEqual(["Waiting on you", "Later (1)"]));
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(shortRow("ask-a").getAttribute("data-inbox-section")).toBe("later");
    expect(within(screen.getByTestId("inbox-pane")).getByText("Snoozing…")).toBeTruthy();

    await act(async () => {
      refusal.reject(
        new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
      );
      await Promise.resolve();
    });

    // The reason and its Retry are in the head that wrote, on the row the reader is still on.
    const head = await waitFor(() => within(screen.getByTestId("inbox-pane")).getByRole("alert"));
    expect(head.textContent).toContain("snoozed_until must be in the future");
    expect(
      within(screen.getByTestId("inbox-pane")).getByRole("button", { name: "Retry" })
    ).toBeTruthy();
    expect(selectedIds()).toEqual(["ask-a"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("a row the reader defers hands the selection to the next one, and the last leaves no ?item=", async () => {
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(address()).toBe("/?item=ask-a");
    fireEvent.change(within(pane).getByLabelText("Snooze CORE-1: Which approach?"), {
      target: { value: "tomorrow" },
    });

    // A row the reader defers is a row they have dealt with: the next one takes its place and
    // the address follows, as it does for a row they answer.
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");

    fireEvent.change(
      within(screen.getByTestId("inbox-pane")).getByLabelText("Snooze CORE-2: Which format?"),
      { target: { value: "tomorrow" } }
    );

    // With the last row deferred the address names none at all: a reload reads it, and one
    // naming a deferred row would open the folded band in front of the reader again.
    await waitFor(() => expect(address()).toBe("/"));
    expect(selectedIds()).toEqual([]);
    expect(screen.queryByTestId("inbox-pane")).toBeNull();
    expect(headings()).toEqual(["Later (2)"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});
