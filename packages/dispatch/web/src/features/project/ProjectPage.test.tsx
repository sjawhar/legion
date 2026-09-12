import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { ProjectPage } from "./ProjectPage";

function renderPage(
  path: string,
  projects = [{ created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 }],
  login = "alice"
) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login });

  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <ProjectPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getMyState, listIssues, listProjectArtifacts, listProjects, view, whoAmI };
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
    documents.view.unmount();
    documents.getMyState.mockRestore();
    documents.listIssues.mockRestore();
    documents.listProjectArtifacts.mockRestore();
    documents.listProjects.mockRestore();
    documents.whoAmI.mockRestore();
  }
});

test("unknown project shows the not-found view", async () => {
  const page = renderPage("/projects/MISSING", []);

  try {
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeTruthy();
  } finally {
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
