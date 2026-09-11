import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { ProjectPage } from "./ProjectPage";

function renderPage(
  path: string,
  projects = [{ created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 }]
) {
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
  return { getMyState, listIssues, listProjectArtifacts, listProjects, view };
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
  }
});
