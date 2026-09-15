import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";
import { Inbox } from "./Inbox";

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
    issue: { key: "CORE-1", title: "Fix the thing" },
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
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({ ask, edits: [], replies: [] });
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
    issue: { key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    edits: [],
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
    issue: { key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T01:00:00Z" },

    waiting_on: "agent",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    edits: [],
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
    expect(screen.getByText("Waiting on session-1")).toBeTruthy();
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
    issue: { key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T02:00:00Z" },

    waiting_on: "agent",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    edits: [],
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
    expect(screen.getByText("session-2 replied")).toBeTruthy();
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
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({ ask: noted, edits: [], replies: [] });
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
    expect(screen.queryByText("session-1 replied")).toBeNull();
    expect(screen.getByText("Waiting on session-1")).toBeTruthy();
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
    const waiting = await screen.findByRole("heading", { name: "Waiting on you" });
    const section = waiting.parentElement;
    if (section === null) throw new Error("Waiting on you section is missing");
    expect(
      within(section)
        .getAllByTestId(/^ask-ask-/)
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
      "Asks from session-1 waiting on you · clear"
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
