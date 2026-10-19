import { expect, spyOn, test } from "bun:test";
import { PROJECT_NAME_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type { ArchitectureSource, Project, RepoProject } from "../api/types";
import { App } from "../app";

/** The mapping routes' server state as the Settings page reaches it: a read answers what the writes
 *  have left, and a write follows the server's default rules (the body's default, else the
 *  mapping's own while its project is unchanged; at most one default per project). */
function mappingServer(initial: RepoProject[]) {
  let rows = initial.map((row) => ({ ...row }));
  const put = (repo: string, input: { project: string; default?: boolean }): RepoProject => {
    const existing = rows.find((row) => row.repo === repo);
    const mapping: RepoProject = {
      created_at: "2026-09-10T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      default: input.default ?? (existing?.project === input.project ? existing.default : false),
      project: input.project,
      repo,
    };
    rows = [
      ...rows
        .filter((row) => row.repo !== repo)
        .map((row) =>
          mapping.default && row.project === mapping.project ? { ...row, default: false } : row
        ),
      mapping,
    ];
    return { ...mapping };
  };
  return {
    list: async (): Promise<RepoProject[]> => rows.map((row) => ({ ...row })),
    put,
    remove: async (repo: string): Promise<void> => {
      rows = rows.filter((row) => row.repo !== repo);
    },
  };
}

function stubMatchMedia(): () => void {
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
  return () => {
    window.matchMedia = originalMatchMedia;
  };
}

test("human users manage repository project mappings from the settings route", async () => {
  const restoreMatchMedia = stubMatchMedia();
  const mapping: RepoProject = {
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    default: false,
    project: "CORE",
    repo: "owner/repo",
  };
  const other: RepoProject = { ...mapping, default: true, repo: "owner/other" };
  const server = mappingServer([mapping, other]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockImplementation(server.list);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" },
  ]);
  const putRepoProject = spyOn(api, "putRepoProject").mockImplementation(async (repo, input) =>
    server.put(repo, input)
  );
  const deleteRepoProject = spyOn(api, "deleteRepoProject").mockImplementation(server.remove);
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
    expect(screen.getByRole("columnheader", { name: "Default" })).toBeDefined();
    const radio = (repo: string) =>
      screen.getByRole("radio", {
        name: `Default repository of CORE: ${repo}`,
      }) as HTMLInputElement;
    expect(radio("owner/other").checked).toBe(true);
    expect(radio("owner/repo").checked).toBe(false);
    fireEvent.click(radio("owner/repo"));
    await waitFor(() =>
      expect(putRepoProject).toHaveBeenLastCalledWith("owner/repo", {
        project: "CORE",
        default: true,
      })
    );
    // The pick moves the project's default at once: one default per project.
    expect(radio("owner/repo").checked).toBe(true);
    expect(radio("owner/other").checked).toBe(false);

    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "Owner/Repo.git" } });
    fireEvent.change(screen.getByLabelText("Project"), { target: { value: "CORE" } });
    fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await waitFor(() =>
      expect(putRepoProject).toHaveBeenLastCalledWith("owner/repo", { project: "CORE" })
    );
    expect(screen.getAllByRole("cell", { name: "owner/repo" })).toHaveLength(1);
    expect(screen.queryByRole("cell", { name: "Owner/Repo.git" })).toBeNull();

    // A mapping issues name is refused by the server, which says how many: the row comes back
    // and the refusal shows.
    deleteRepoProject.mockRejectedValueOnce(
      new ApiError(409, {
        code: "REPO_PROJECT_IN_USE",
        error: "1 issue(s) of CORE name owner/repo as their repository; change their repo first",
      })
    );
    fireEvent.click(screen.getByRole("button", { name: "Delete mapping for owner/repo" }));
    expect(
      await screen.findByText(
        "1 issue(s) of CORE name owner/repo as their repository; change their repo first"
      )
    ).toBeDefined();
    expect(screen.getByRole("cell", { name: "owner/repo" })).toBeDefined();

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
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    putRepoProject.mockRestore();
    deleteRepoProject.mockRestore();
    restoreMatchMedia();
  }
});

// A mapping write's response describes one row, not the project's mappings as they stand when it
// arrives: the page reads them again once the write settles, so a response that arrives after the
// page already read a newer default does not put the older one back.
test("a delayed mapping response does not undo a newer default the page has read", async () => {
  const restoreMatchMedia = stubMatchMedia();
  const row = (repo: string, isDefault: boolean): RepoProject => ({
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    default: isDefault,
    project: "CORE",
    repo,
  });
  const server = mappingServer([row("owner/repo", false), row("owner/other", true)]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockImplementation(server.list);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" },
  ]);
  // The server saves the pick at once; its answer reaches the page only when the test says.
  let answer: (() => void) | undefined;
  const putRepoProject = spyOn(api, "putRepoProject").mockImplementation((repo, input) => {
    const saved = server.put(repo, input);
    return new Promise<RepoProject>((resolve) => {
      answer = () => resolve(saved);
    });
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const radio = (repo: string) =>
    screen.getByRole("radio", { name: `Default repository of CORE: ${repo}` }) as HTMLInputElement;

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );
    await screen.findByRole("radio", { name: "Default repository of CORE: owner/repo" });
    fireEvent.click(radio("owner/repo"));
    await waitFor(() => expect(putRepoProject).toHaveBeenCalledTimes(1));
    // Another browser makes owner/other the default again, and this page reads it (as a mapping
    // event would have it do) while its own write's answer is still on the way.
    server.put("owner/other", { default: true, project: "CORE" });
    await act(() => queryClient.invalidateQueries({ queryKey: ["repo-projects"] }));
    await waitFor(() => expect(radio("owner/other").checked).toBe(true));
    const reads = listRepoProjects.mock.calls.length;
    act(() => answer?.());
    await waitFor(() => expect(listRepoProjects.mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() => expect(radio("owner/other").checked).toBe(true));
    expect(radio("owner/repo").checked).toBe(false);
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    putRepoProject.mockRestore();
    restoreMatchMedia();
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
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
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
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getDeliverySettings.mockRestore();
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
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
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
    expect(screen.getByLabelText("Name")).toHaveProperty("maxLength", PROJECT_NAME_MAX);
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
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getDeliverySettings.mockRestore();
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
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
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
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getDeliverySettings.mockRestore();
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
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources")
    .mockResolvedValueOnce([])
    .mockResolvedValue([architectureSource]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
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
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getDeliverySettings.mockRestore();
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
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
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
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});
