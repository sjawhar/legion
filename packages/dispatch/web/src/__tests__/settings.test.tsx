import { expect, spyOn, test } from "bun:test";
import { PROJECT_NAME_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type {
  ArchitectureSource,
  Project,
  ProjectDefaultAssignee,
  RepoProject,
} from "../api/types";
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
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listDefaults.mockRestore();
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
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listDefaults.mockRestore();
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
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listDefaults.mockRestore();
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
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listDefaults.mockRestore();
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
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listDefaults.mockRestore();
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
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listDefaults.mockRestore();
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
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listDefaults.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});

/** The default-assignee routes' server state as the Settings page reaches it: a read answers
 *  what the writes have left, and `sent` records each write's assignee as it arrives. While
 *  `hold()`'s release has not been called, a write that arrives waits before it lands. */
function defaultAssigneeServer(initial: ProjectDefaultAssignee[]) {
  let rows = initial.map((row) => ({ ...row }));
  const sent: (string | null)[] = [];
  let gate: Promise<void> | undefined;
  return {
    hold: (): (() => void) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      gate = promise;
      return () => {
        gate = undefined;
        resolve();
      };
    },
    list: async (): Promise<ProjectDefaultAssignee[]> => rows.map((row) => ({ ...row })),
    put: async (
      project: string,
      { assignee }: { assignee: string | null }
    ): Promise<ProjectDefaultAssignee> => {
      sent.push(assignee);
      await gate;
      rows = rows.filter((row) => row.project !== project);
      if (assignee !== null) {
        rows.push({ assignee, project });
      }
      return { assignee, project };
    },
    rows: () => rows,
    sent,
  };
}

/** Renders the Settings route with every read but the default assignees and the people list
 *  stubbed, listing `projects`. The returned function unmounts it and restores the stubs. */
function renderSettings(projects: string[]): () => void {
  const stubs = [
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" }),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listRepoProjects").mockResolvedValue([]),
    spyOn(api, "listAgentTokens").mockResolvedValue([]),
    spyOn(api, "getCredentialPending").mockResolvedValue(null),
    spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getDeliverySettings").mockResolvedValue(null),
    spyOn(api, "listProjects").mockResolvedValue(
      projects.map((key) => ({ created_at: "2026-09-01T00:00:00Z", key, name: key }))
    ),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={["/settings"]}>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return () => {
    cleanup();
    queryClient.clear();
    for (const stub of stubs) {
      stub.mockRestore();
    }
  };
}

const defaultAssigneeControl = async (project: string): Promise<HTMLSelectElement> =>
  (await screen.findByRole("combobox", {
    name: `Default assignee of ${project}`,
  })) as HTMLSelectElement;

test("default assignee picks made while one is saving show at once and go one at a time, the last one kept", async () => {
  const server = defaultAssigneeServer([{ assignee: "alice@example.com", project: "CORE" }]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockImplementation(server.list);
  const putDefault = spyOn(api, "putProjectDefaultAssignee").mockImplementation(server.put);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice@example.com" },
    { login: "bob@example.com" },
    { login: "carol@example.com" },
  ]);
  const done = renderSettings(["CORE"]);

  try {
    const section = within(await screen.findByRole("region", { name: "Projects" }));
    // A default set on the project an unmapped repository falls back to is what a reader has to
    // weigh while setting one, so the consequence sits beside the control.
    expect(section.getByText(/no project mapping/).textContent).toContain("default project");

    const control = await defaultAssigneeControl("CORE");
    await waitFor(() => expect(control.value).toBe("alice@example.com"));
    expect(control.disabled).toBe(false);
    // Nobody is listed until a reader reaches for the picker.
    expect(listUsers).not.toHaveBeenCalled();

    fireEvent.focus(control);
    await waitFor(() =>
      expect(within(control).getByRole("option", { name: "carol@example.com" })).toBeDefined()
    );
    expect(
      within(control)
        .getAllByRole("option")
        .map((option) => option.textContent)
    ).toEqual(["Unassigned", "alice@example.com", "bob@example.com", "carol@example.com"]);

    const release = server.hold();
    fireEvent.change(control, { target: { value: "bob@example.com" } });
    expect(control.value).toBe("bob@example.com");
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com"]));

    // Two more picks while bob's save is held: each shows at once, neither is sent yet.
    fireEvent.change(control, { target: { value: "" } });
    expect(control.value).toBe("");
    fireEvent.change(control, { target: { value: "carol@example.com" } });
    expect(control.value).toBe("carol@example.com");
    await act(async () => {});
    expect(putDefault).toHaveBeenCalledTimes(1);

    await act(async () => release());
    // Only the newest queued pick follows the held save; the Unassigned pick between is dropped.
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com", "carol@example.com"]));
    await waitFor(() => expect(listDefaults.mock.calls.length).toBeGreaterThan(1));
    expect(control.value).toBe("carol@example.com");
    expect(putDefault).toHaveBeenCalledTimes(2);
    expect(server.rows()).toEqual([{ assignee: "carol@example.com", project: "CORE" }]);

    // Clearing it leaves the project with no default, and the control at Unassigned.
    fireEvent.change(control, { target: { value: "" } });
    await waitFor(() => expect(server.rows()).toEqual([]));
    await waitFor(() => expect(putDefault).toHaveBeenLastCalledWith("CORE", { assignee: null }));
    expect(control.value).toBe("");
  } finally {
    done();
    listDefaults.mockRestore();
    putDefault.mockRestore();
    listUsers.mockRestore();
  }
});

test("a refused default assignee puts the row back, shows the server's reason in that row, and Retry resends it", async () => {
  const server = defaultAssigneeServer([]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockImplementation(server.list);
  const putDefault = spyOn(api, "putProjectDefaultAssignee")
    .mockRejectedValueOnce(
      new ApiError(400, {
        code: "ASSIGNEE_NOT_ALLOWED",
        error: "bob@example.com has never signed in",
      })
    )
    .mockImplementation(server.put);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "bob@example.com" }]);
  const done = renderSettings(["CORE", "OPS"]);

  try {
    const core = await defaultAssigneeControl("CORE");
    const ops = await defaultAssigneeControl("OPS");
    const coreCell = within(core.closest("td") as HTMLElement);
    const opsCell = within(ops.closest("td") as HTMLElement);
    await waitFor(() => expect(core.disabled).toBe(false));
    fireEvent.focus(core);
    await waitFor(() =>
      expect(within(core).getByRole("option", { name: "bob@example.com" })).toBeDefined()
    );

    fireEvent.change(core, { target: { value: "bob@example.com" } });
    await waitFor(() =>
      expect(coreCell.getByRole("alert").textContent).toContain(
        "bob@example.com has never signed in"
      )
    );
    // The refused pick is not left on screen: the row reads what the server holds.
    expect(core.value).toBe("");

    // A save on another row neither clears nor takes over this row's error.
    fireEvent.change(ops, { target: { value: "bob@example.com" } });
    await waitFor(() =>
      expect(server.rows()).toEqual([{ assignee: "bob@example.com", project: "OPS" }])
    );
    expect(ops.value).toBe("bob@example.com");
    expect(opsCell.queryByRole("alert")).toBeNull();
    expect(coreCell.getByRole("alert").textContent).toContain(
      "bob@example.com has never signed in"
    );

    fireEvent.click(coreCell.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(coreCell.queryByRole("alert")).toBeNull());
    expect(putDefault.mock.calls).toEqual([
      ["CORE", { assignee: "bob@example.com" }],
      ["OPS", { assignee: "bob@example.com" }],
      ["CORE", { assignee: "bob@example.com" }],
    ]);
    await waitFor(() => expect(core.value).toBe("bob@example.com"));
    expect(server.rows()).toContainEqual({ assignee: "bob@example.com", project: "CORE" });
  } finally {
    done();
    listDefaults.mockRestore();
    putDefault.mockRestore();
    listUsers.mockRestore();
  }
});

test("a default assignee the page has not read is disabled and never reads Unassigned", async () => {
  const read = Promise.withResolvers<ProjectDefaultAssignee[]>();
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockReturnValueOnce(read.promise);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "alice@example.com" }]);
  const done = renderSettings(["CORE"]);
  const optionNames = (control: HTMLSelectElement) =>
    within(control)
      .getAllByRole("option")
      .map((option) => option.textContent);

  try {
    const section = within(await screen.findByRole("region", { name: "Projects" }));
    const control = await defaultAssigneeControl("CORE");
    // Still reading: nobody knows yet whether CORE names a default.
    expect(control.disabled).toBe(true);
    expect(control.value).not.toBe("");
    expect(optionNames(control)).toEqual(["Loading…"]);

    await act(async () => read.reject(new ApiError(500, { code: "INTERNAL", error: "boom" })));
    await waitFor(() =>
      expect(section.getByRole("alert").textContent).toContain(
        "Couldn't load the default assignees."
      )
    );
    expect(control.disabled).toBe(true);
    expect(control.value).not.toBe("");
    expect(optionNames(control)).toEqual(["Unavailable"]);

    listDefaults.mockResolvedValue([{ assignee: "alice@example.com", project: "CORE" }]);
    fireEvent.click(section.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(control.disabled).toBe(false));
    expect(control.value).toBe("alice@example.com");
    expect(section.queryByRole("alert")).toBeNull();
  } finally {
    done();
    listDefaults.mockRestore();
    listUsers.mockRestore();
  }
});
