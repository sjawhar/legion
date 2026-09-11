import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type { IssueSummary, Project } from "../api/types";
import { Sidebar } from "../features/sidebar/Sidebar";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 1,
    parent: null,
    status: "todo",
    title: "Pinned work",
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

function project(overrides: Partial<Project> = {}): Project {
  return {
    created_at: "2026-09-10T00:00:00Z",
    key: "CORE",
    name: "Core",
    open_asks: 3,
    ...overrides,
  };
}

function renderSidebar(pathname = "/") {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([
    {
      anchor: null,
      answer: null,
      author: { id: "alice", kind: "user" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: "CORE-1",
      multiple: false,
      opened_event_id: 1,
      options: [],
      question: "Ship?",
      state: "open",
      urgency: "med",
    },
    {
      anchor: null,
      answer: null,
      author: { id: "alice", kind: "user" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-2",
      issue_key: "OPS-1",
      multiple: false,
      opened_event_id: 2,
      options: [],
      question: "Review?",
      state: "open",
      urgency: "med",
    },
  ]);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(undefined as never);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([issue()]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    project(),
    project({ key: "OPS", name: "Operations", open_asks: 0 }),
  ]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: [pathname] },
      createElement(
        QueryClientProvider,
        { client: queryClient },
        createElement(Sidebar, { user: { kind: "user", login: "alice" } })
      )
    )
  );
  return { getInbox, getIssue, listIssues, listProjects, view };
}

test("sidebar lists Inbox, Pinned, Projects, and Settings with open-ask badges and renders no full issue list", async () => {
  const sidebar = renderSidebar();

  try {
    await screen.findByRole("link", { name: /CORE.*Core/ });
    expect(screen.getByRole("link", { name: /Inbox/ }).textContent).toContain("2");
    expect(screen.getByRole("heading", { name: "Pinned" })).toBeTruthy();
    expect(screen.getByRole("link", { name: /CORE-1.*Pinned work/ }).textContent).toContain("1");
    expect(screen.getByRole("heading", { name: "Projects" })).toBeTruthy();
    expect(screen.getByRole("link", { name: /CORE.*Core/ }).textContent).toContain("3");
    expect(screen.getByRole("link", { name: /OPS.*Operations/ })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Settings" })).toBeTruthy();
  } finally {
    sidebar.view.unmount();
    sidebar.getInbox.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

test("sidebar marks the current project", async () => {
  const sidebar = renderSidebar("/projects/CORE/documents");

  try {
    const core = await screen.findByRole("link", { name: /CORE.*Core/ });
    expect(core.getAttribute("aria-current")).toBe("page");
  } finally {
    sidebar.view.unmount();
    sidebar.getInbox.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

test("sidebar never requests the full issue list", async () => {
  const sidebar = renderSidebar();

  try {
    await screen.findByRole("link", { name: /CORE.*Core/ });
    expect(sidebar.listIssues).toHaveBeenCalledTimes(1);
    expect(sidebar.listIssues).toHaveBeenCalledWith({ pinned: true });
    expect(sidebar.getIssue).not.toHaveBeenCalled();
  } finally {
    sidebar.view.unmount();
    sidebar.getInbox.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});
