import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type { ArchitectureSource, Project, RepoProject } from "../api/types";
import { App } from "../app";

test("human users manage repository project mappings from the settings route", async () => {
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const mapping: RepoProject = {
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    project: "CORE",
    repo: "owner/repo",
  };
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([mapping]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" },
  ]);
  const putRepoProject = spyOn(api, "putRepoProject").mockResolvedValue({
    ...mapping,
    repo: "owner/repo",
  });
  const deleteRepoProject = spyOn(api, "deleteRepoProject").mockResolvedValue();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect(await screen.findByRole("heading", { name: "Repositories → Projects" })).toBeDefined();
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("href")).toBe("/settings");
    expect(screen.getByRole("cell", { name: "owner/repo" })).toBeDefined();

    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "Owner/Repo.git" } });
    fireEvent.change(screen.getByLabelText("Project"), { target: { value: "CORE" } });
    fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await waitFor(() =>
      expect(putRepoProject).toHaveBeenLastCalledWith("owner/repo", { project: "CORE" })
    );
    expect(screen.getAllByRole("cell", { name: "owner/repo" })).toHaveLength(1);
    expect(screen.queryByRole("cell", { name: "Owner/Repo.git" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Delete mapping for owner/repo" }));
    await waitFor(() => expect(deleteRepoProject).toHaveBeenLastCalledWith("owner/repo"));
    await waitFor(() => expect(screen.queryByRole("cell", { name: "owner/repo" })).toBeNull());
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putRepoProject.mockRestore();
    deleteRepoProject.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
});

test("repository settings retries a failed mapping query", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects")
    .mockRejectedValueOnce(new Error("network"))
    .mockResolvedValueOnce([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" },
  ]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect((await screen.findByRole("alert")).textContent).toContain(
      "Couldn't load repository mappings."
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalledTimes(2));
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
  }
});

test("a human creates a project from Settings and it appears in the mappings selector", async () => {
  const core: Project = { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" };
  const created: Project = { created_at: "2026-09-10T00:00:00Z", key: "QA", name: "Quality" };
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects")
    .mockResolvedValueOnce([core])
    .mockResolvedValueOnce([core, created]);
  const createProject = spyOn(api, "createProject").mockResolvedValue(created);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect(await screen.findByRole("cell", { name: "CORE" })).toBeDefined();
    fireEvent.change(screen.getByLabelText("Key"), { target: { value: "qa" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Quality" } });
    fireEvent.click(screen.getByRole("button", { name: "New project" }));

    await waitFor(() =>
      expect(createProject).toHaveBeenLastCalledWith({ key: "QA", name: "Quality" })
    );
    await waitFor(() => expect(listProjects).toHaveBeenCalledTimes(2));
    expect(await screen.findByRole("cell", { name: "QA" })).toBeDefined();
    expect(
      within(screen.getByRole("combobox", { name: "Project" })).getByRole("option", {
        name: "QA · Quality",
      })
    ).toBeDefined();
    expect(screen.getByLabelText("Key")).toHaveProperty("value", "");
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    createProject.mockRestore();
  }
});

test("creating a project with a taken key shows the server's error inline", async () => {
  const core: Project = { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" };
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const createProject = spyOn(api, "createProject").mockRejectedValue(
    new ApiError(409, { code: "PROJECT_EXISTS", error: "a project with this key already exists" })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect(await screen.findByRole("cell", { name: "CORE" })).toBeDefined();
    fireEvent.change(screen.getByLabelText("Key"), { target: { value: "CORE" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Core Again" } });
    fireEvent.click(screen.getByRole("button", { name: "New project" }));

    expect((await screen.findByRole("alert")).textContent).toContain(
      "a project with this key already exists"
    );
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    createProject.mockRestore();
  }
});

const architectureSource: ArchitectureSource = {
  branch: "main",
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: null,
  last_error: null,
  last_sync_at: null,
  project: "CORE",
  repo: "legion/arch",
};

test("a human adds an architecture source and the verified row appears", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources")
    .mockResolvedValueOnce([])
    .mockResolvedValue([architectureSource]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" },
  ]);
  const putArchitectureSource = spyOn(api, "putArchitectureSource").mockResolvedValue(
    architectureSource
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    expect(section.getByText("No architecture sources yet.")).toBeDefined();

    fireEvent.change(section.getByLabelText("Source project"), { target: { value: "CORE" } });
    fireEvent.change(section.getByLabelText("Source repository"), {
      target: { value: "legion/arch" },
    });
    fireEvent.click(section.getByRole("button", { name: "Add source" }));
    await waitFor(() =>
      expect(putArchitectureSource).toHaveBeenLastCalledWith("CORE", {
        branch: "main",
        repo: "legion/arch",
      })
    );
    await waitFor(() => expect(section.getByRole("cell", { name: "legion/arch" })).toBeDefined());
    expect(section.getByRole("cell", { name: "Access verified" })).toBeDefined();
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});

test("a failed architecture source access check shows the server's reason inline", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" },
  ]);
  const putArchitectureSource = spyOn(api, "putArchitectureSource").mockRejectedValue(
    new ApiError(409, {
      code: "SOURCE_ACCESS",
      error: "the GitHub App is not installed on legion/arch",
    })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    fireEvent.change(section.getByLabelText("Source project"), { target: { value: "CORE" } });
    fireEvent.change(section.getByLabelText("Source repository"), {
      target: { value: "legion/arch" },
    });
    fireEvent.click(section.getByRole("button", { name: "Add source" }));

    await waitFor(() =>
      expect(section.getByRole("alert").textContent).toContain(
        "the GitHub App is not installed on legion/arch"
      )
    );
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});
