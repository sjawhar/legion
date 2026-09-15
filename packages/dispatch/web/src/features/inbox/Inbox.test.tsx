import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxRow, Issue } from "../../api/types";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { Inbox } from "./Inbox";

// Every Inbox reads the signed-in login: the default view is the viewer's own issues, and the
// server echoes GitHub's casing ("Alice") while issues carry the lowercase login.
let whoAmI: Mock<typeof api.whoAmI>;
beforeEach(() => {
  window.localStorage.clear();
  whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "Alice" });
});
afterEach(() => {
  whoAmI.mockRestore();
});

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
    question: "Does this design need review?",
    priority: null,
    state: "open",
    waiting_on: "human",
    urgency: "med",
  };
}

function issueAsk(overrides: Partial<InboxRow> = {}): InboxRow {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id: "ask-a",
    issue: { assignee: "alice", key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which approach?",
    state: "open",
    waiting_on: "human",
    priority: null,
    urgency: "med",
    ...overrides,
  };
}

test("Inbox labels an artifact-owned ask with its project and document page link", async () => {
  const ask = artifactAsk();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([ask]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask,
    edits: [],
    followers: [],
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
    edits: [],
    followers: [],
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
    edits: [],
    followers: [],
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
    expect(screen.getByText("Waiting on session:session-…")).toBeTruthy();
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
    edits: [],
    followers: [],
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
    expect(screen.getByText("session:session-… replied")).toBeTruthy();
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
    edits: [],
    followers: [],
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
    expect(screen.queryByText("session:session-… replied")).toBeNull();
    expect(screen.getByText("Waiting on session:session-…")).toBeTruthy();
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
    edits: [],
    followers: [],
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
    edits: [],
    followers: [],
    replies: [],
  }));
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/w",
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
    edits: [],
    followers: [],
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
    edits: [],
    followers: [],
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
    expect(within(emptyState).getByText("No open asks from idle-session")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Clear agent filter" }).getAttribute("href")).toBe("/");
    expect(screen.queryByText("Which approach?")).toBeNull();
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a draft, its option, and focus survive the ask moving to Waiting on agents, and a new ask appears at once", async () => {
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
    edits: [],
    followers: [],
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

    // An agent's progress note hands the turn to the agent while a P0 ask arrives above.
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
    expect(
      screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)
    ).toEqual(["Waiting on you", "Waiting on agents"]);
    expect(screen.getByTestId("ask-ask-c")).toBeTruthy();
    expect(moved.closest("[data-inbox-section]")?.getAttribute("data-inbox-section")).toBe("agent");
    expect(screen.getByTestId("turn-ask-b").textContent).toBe("Waiting on session:session-…");
  } finally {
    view.unmount();
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
    edits: [],
    followers: [],
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
    edits: [],
    followers: [],
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
    edits: [],
    followers: [],
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

function renderInbox(route = "/") {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

function mockAskReads(rows: readonly InboxRow[]) {
  return spyOn(api, "getAsk").mockImplementation(async (id: string) => {
    const ask = rows.find((row) => row.id === id);
    if (ask === undefined) throw new Error(`no fixture for ${id}`);
    return { ask, edits: [], followers: [], replies: [] };
  });
}

function headings(): string[] {
  return screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent ?? "");
}

test("a fresh login lands on Mine: their issues' asks, then an Unassigned band with Assign to me on issue rows only", async () => {
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
  const document = artifactAsk();
  const rows = [mine, bobs, orphan, document];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which approach?");
    expect(screen.getByRole("button", { name: "Mine" }).getAttribute("aria-pressed")).toBe("true");
    expect(headings()).toEqual(["Waiting on you", "Unassigned"]);
    expect(screen.queryByText("Bob's question?")).toBeNull();
    const rowOf = (id: string) => {
      const row = screen.getByTestId(`ask-${id}`).closest<HTMLElement>("[data-inbox-row]");
      if (row === null) throw new Error(`${id} row missing`);
      return row;
    };
    expect(rowOf("ask-mine").getAttribute("data-inbox-section")).toBe("human");
    expect(rowOf("ask-orphan").getAttribute("data-inbox-section")).toBe("unassigned");
    expect(rowOf("ask-1").getAttribute("data-inbox-section")).toBe("unassigned");
    expect(
      within(rowOf("ask-orphan")).getByRole("button", { name: "Assign CORE-3 to me" })
    ).toBeTruthy();
    // A document ask has no assignee to set.
    expect(within(rowOf("ask-1")).queryByRole("button", { name: /Assign .* to me/ })).toBeNull();
    expect(screen.getAllByRole("button", { name: /Assign .* to me/ })).toHaveLength(1);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Everyone shows every open ask and is remembered for the login; ?view= wins over the memory", async () => {
  const mine = issueAsk({ id: "ask-mine" });
  const bobs = issueAsk({
    id: "ask-bob",
    issue: { assignee: "bob", key: "CORE-2", title: "Bob's issue" },
    issue_key: "CORE-2",
    question: "Bob's question?",
  });
  const rows = [mine, bobs];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const first = renderInbox();
  try {
    await screen.findByText("Which approach?");
    fireEvent.click(screen.getByRole("button", { name: "Everyone" }));
    await screen.findByText("Bob's question?");
    expect(screen.getByRole("button", { name: "Everyone" }).getAttribute("aria-pressed")).toBe(
      "true"
    );
    expect(headings()).toEqual(["Waiting on you"]);
    expect(window.localStorage.getItem(userPreferenceStorageKey("Alice", "inbox.view"))).toBe(
      "everyone"
    );
  } finally {
    first.unmount();
  }
  const remembered = renderInbox();
  try {
    await screen.findByText("Bob's question?");
    expect(screen.getByRole("button", { name: "Everyone" }).getAttribute("aria-pressed")).toBe(
      "true"
    );
  } finally {
    remembered.unmount();
  }
  const fromUrl = renderInbox("/?view=mine");
  try {
    await screen.findByText("Which approach?");
    expect(screen.queryByText("Bob's question?")).toBeNull();
    expect(screen.getByRole("button", { name: "Mine" }).getAttribute("aria-pressed")).toBe("true");
  } finally {
    fromUrl.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Assign to me PATCHes the viewer's lowercase login and moves the row into Mine at once", async () => {
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
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(() => save.promise);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Orphan question?");
    expect(headings()).toEqual(["Unassigned"]);
    fireEvent.click(screen.getByRole("button", { name: "Assign CORE-3 to me" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }));
    await waitFor(() => expect(headings()).toEqual(["Waiting on you"]));
    expect(
      screen
        .getByTestId("ask-ask-orphan")
        .closest("[data-inbox-section]")
        ?.getAttribute("data-inbox-section")
    ).toBe("human");
    expect(screen.queryByRole("button", { name: "Assign CORE-3 to me" })).toBeNull();
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Mine's Blocked on you counts the viewer's and the unassigned asks; Everyone counts them all", async () => {
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
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which approach?");
    expect(screen.getByText(/Blocked on you: 2 items/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Everyone" }));
    await screen.findByText("Bob's question?");
    expect(screen.getByText(/Blocked on you: 3 items/)).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});
