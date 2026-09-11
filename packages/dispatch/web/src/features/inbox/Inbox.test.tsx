import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask } from "../../api/types";
import { Inbox } from "./Inbox";

function artifactAsk(): Ask {
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
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Does this design need review?",
    state: "open",
    urgency: "med",
  };
}

function issueAsk(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id: "ask-a",
    issue: { key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which approach?",
    state: "open",
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

test("Inbox renders a flat list with no section heading when every row needs the viewer", async () => {
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
    expect(screen.queryAllByRole("heading", { level: 2 })).toHaveLength(0);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox splits rows needing the viewer from rows waiting on an agent, and chips the waiting row", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T01:00:00Z" },
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
    expect(headings).toEqual(["Needs you", "Waiting on agents"]);
    expect(screen.getByText("Waiting on session-1")).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox chips a Needs-you row whose last reply came from an agent", async () => {
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
