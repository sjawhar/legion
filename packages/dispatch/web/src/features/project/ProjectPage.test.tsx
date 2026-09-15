import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ProjectPage } from "./ProjectPage";

function inboxRow(overrides: Partial<InboxRow> = {}): InboxRow {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue: { key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    priority: null,
    question: "Which approach?",
    state: "open",
    waiting_on: "human",
    urgency: "med",
    ...overrides,
  };
}

function renderPage(
  path: string,
  projects = [{ created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 }],
  login = "alice",
  inboxRows: InboxRow[] = []
) {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(inboxRows);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login });

  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ProjectPage />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getInbox, getMyState, listIssues, listProjectArtifacts, listProjects, view, whoAmI };
}

test("renders header, tabs, and the Issues panel; /documents selects Documents", async () => {
  const page = renderPage("/projects/CORE");

  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByText("CORE", { exact: true })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Issues" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tabpanel", { name: "Issues" })).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: "Documents" }));
    expect(screen.getByRole("tab", { name: "Documents" }).getAttribute("aria-selected")).toBe(
      "true"
    );
    expect(screen.getByRole("tabpanel", { name: "Documents" })).toBeTruthy();
  } finally {
    page.getInbox.mockRestore();
    page.view.unmount();
    page.getMyState.mockRestore();
    page.listIssues.mockRestore();
    page.listProjectArtifacts.mockRestore();
    page.listProjects.mockRestore();
    page.whoAmI.mockRestore();
  }

  const documents = renderPage("/projects/CORE/documents");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("tab", { name: "Documents" }).getAttribute("aria-selected")).toBe(
      "true"
    );
  } finally {
    documents.getInbox.mockRestore();
    documents.view.unmount();
    documents.getMyState.mockRestore();
    documents.listIssues.mockRestore();
    documents.listProjectArtifacts.mockRestore();
    documents.listProjects.mockRestore();
    documents.whoAmI.mockRestore();
  }
});

test("shows a compact blocker link only when asks await the viewer", async () => {
  const waiting = renderPage("/projects/CORE", undefined, "alice", [inboxRow()]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("link", { name: "Blocked on you · 1" }).getAttribute("href")).toBe("/");
    expect(screen.queryByText(/oldest/)).toBeNull();
  } finally {
    waiting.getInbox.mockRestore();
    waiting.getMyState.mockRestore();
    waiting.listIssues.mockRestore();
    waiting.listProjectArtifacts.mockRestore();
    waiting.listProjects.mockRestore();
    waiting.view.unmount();
    waiting.whoAmI.mockRestore();
  }

  const clear = renderPage("/projects/CORE", undefined, "alice", [
    inboxRow({
      last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T01:00:00Z" },

      waiting_on: "agent",
    }),
  ]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.queryByRole("link", { name: /Blocked on you/ })).toBeNull();
  } finally {
    clear.getInbox.mockRestore();
    clear.getMyState.mockRestore();
    clear.listIssues.mockRestore();
    clear.listProjectArtifacts.mockRestore();
    clear.listProjects.mockRestore();
    clear.view.unmount();
    clear.whoAmI.mockRestore();
  }
});

test("unknown project shows the not-found view", async () => {
  const page = renderPage("/projects/MISSING", []);

  try {
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeTruthy();
  } finally {
    page.getInbox.mockRestore();
    page.view.unmount();
    page.getMyState.mockRestore();
    page.listIssues.mockRestore();
    page.listProjectArtifacts.mockRestore();
    page.listProjects.mockRestore();
    page.whoAmI.mockRestore();
  }
});

test("keeps the issue view preference separate for each signed-in user", async () => {
  const aliceStorageKey = "dispatch.project.issue-view:alice";
  const bobStorageKey = "dispatch.project.issue-view:bob";
  window.localStorage.setItem(aliceStorageKey, "board");
  window.localStorage.setItem(bobStorageKey, "list");

  const alice = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe("true");
  } finally {
    alice.getInbox.mockRestore();
    alice.view.unmount();
    alice.getMyState.mockRestore();
    alice.listIssues.mockRestore();
    alice.listProjectArtifacts.mockRestore();
    alice.listProjects.mockRestore();
    alice.whoAmI.mockRestore();
  }

  const bob = renderPage("/projects/CORE", undefined, "bob");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
  } finally {
    bob.getInbox.mockRestore();
    bob.view.unmount();
    bob.getMyState.mockRestore();
    bob.listIssues.mockRestore();
    bob.listProjectArtifacts.mockRestore();
    bob.listProjects.mockRestore();
    bob.whoAmI.mockRestore();
    window.localStorage.removeItem(aliceStorageKey);
    window.localStorage.removeItem(bobStorageKey);
  }
});

test("the Icebox & Done toggle lives in Board view only and persists per signed-in user", async () => {
  const viewKey = "dispatch.project.issue-view:alice";
  const edgesKey = "dispatch.project.board-edges:alice";
  window.localStorage.setItem(viewKey, "board");

  const first = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    const toggle = await screen.findByRole("button", { name: "Show Icebox & Done" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    expect(toggle.classList.contains("min-h-11")).toBe(true);
    fireEvent.click(toggle);
    expect(
      screen.getByRole("button", { name: "Hide Icebox & Done" }).getAttribute("aria-pressed")
    ).toBe("true");
    expect(window.localStorage.getItem(edgesKey)).toBe("shown");

    fireEvent.click(screen.getByRole("button", { name: "List" }));
    expect(screen.queryByRole("button", { name: /Icebox & Done/ })).toBeNull();
  } finally {
    first.getInbox.mockRestore();
    first.view.unmount();
    first.getMyState.mockRestore();
    first.listIssues.mockRestore();
    first.listProjectArtifacts.mockRestore();
    first.listProjects.mockRestore();
    first.whoAmI.mockRestore();
  }

  window.localStorage.setItem(viewKey, "board");
  const second = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(
      (await screen.findByRole("button", { name: "Hide Icebox & Done" })).getAttribute(
        "aria-pressed"
      )
    ).toBe("true");
  } finally {
    second.getInbox.mockRestore();
    second.view.unmount();
    second.getMyState.mockRestore();
    second.listIssues.mockRestore();
    second.listProjectArtifacts.mockRestore();
    second.listProjects.mockRestore();
    second.whoAmI.mockRestore();
  }

  const bob = renderPage("/projects/CORE", undefined, "bob");
  try {
    window.localStorage.setItem("dispatch.project.issue-view:bob", "board");
    await screen.findByRole("heading", { name: "Core" });
    fireEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(
      (await screen.findByRole("button", { name: "Show Icebox & Done" })).getAttribute(
        "aria-pressed"
      )
    ).toBe("false");
  } finally {
    bob.getInbox.mockRestore();
    bob.view.unmount();
    bob.getMyState.mockRestore();
    bob.listIssues.mockRestore();
    bob.listProjectArtifacts.mockRestore();
    bob.listProjects.mockRestore();
    bob.whoAmI.mockRestore();
    window.localStorage.removeItem(viewKey);
    window.localStorage.removeItem(edgesKey);
    window.localStorage.removeItem("dispatch.project.issue-view:bob");
    window.localStorage.removeItem("dispatch.project.board-edges:bob");
  }
});

test("v toggles List and Board on the issues tab and writes the preference; it is inert on Documents", async () => {
  const viewKey = "dispatch.project.issue-view:alice";
  const page = renderPage("/projects/CORE");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");

    fireEvent.keyDown(document.body, { key: "v" });
    expect(screen.getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe("true");
    expect(window.localStorage.getItem(viewKey)).toBe("board");

    fireEvent.keyDown(document.body, { key: "v" });
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
    expect(window.localStorage.getItem(viewKey)).toBe("list");
  } finally {
    page.getInbox.mockRestore();
    page.view.unmount();
    page.getMyState.mockRestore();
    page.listIssues.mockRestore();
    page.listProjectArtifacts.mockRestore();
    page.listProjects.mockRestore();
    page.whoAmI.mockRestore();
    window.localStorage.removeItem(viewKey);
  }

  const documents = renderPage("/projects/CORE/documents");
  try {
    await screen.findByRole("heading", { name: "Core" });
    fireEvent.keyDown(document.body, { key: "v" });
    expect(screen.queryByRole("button", { name: "Board" })).toBeNull();
    expect(window.localStorage.getItem(viewKey)).toBeNull();
  } finally {
    documents.getInbox.mockRestore();
    documents.view.unmount();
    documents.getMyState.mockRestore();
    documents.listIssues.mockRestore();
    documents.listProjectArtifacts.mockRestore();
    documents.listProjects.mockRestore();
    documents.whoAmI.mockRestore();
  }
});
