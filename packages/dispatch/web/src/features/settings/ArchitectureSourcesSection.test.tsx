import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { ApiError, api } from "../../api/client";
import type { ArchitectureSource } from "../../api/types";
import { ArchitectureSourcesSection } from "./ArchitectureSourcesSection";

const architectureSource: ArchitectureSource = {
  branch: "main",
  components: 0,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: null,
  last_error: null,
  last_sync_at: null,
  project: "CORE",
  repo: "legion/arch",
};

/** The architecture source routes' server state as the Settings page reaches it: one row per
 *  project and repository, listed in the order they were added; a save of a repository the
 *  project already has changes its branch and resets its bookkeeping, as the server does. */
function sourceServer(initial: ArchitectureSource[]) {
  let rows = initial.map((row) => ({ ...row }));
  const same = (row: ArchitectureSource, project: string, repo: string) =>
    row.project === project && row.repo === repo;
  return {
    list: async (): Promise<ArchitectureSource[]> => rows.map((row) => ({ ...row })),
    put: async (
      project: string,
      repo: string,
      input: { branch: string }
    ): Promise<ArchitectureSource> => {
      const saved: ArchitectureSource = {
        ...architectureSource,
        branch: input.branch,
        components: rows.find((row) => same(row, project, repo))?.components ?? 0,
        project,
        repo,
      };
      rows = rows.some((row) => same(row, project, repo))
        ? rows.map((row) => (same(row, project, repo) ? saved : row))
        : [...rows, saved];
      return { ...saved };
    },
    remove: async (project: string, repo: string): Promise<void> => {
      rows = rows.filter((row) => !same(row, project, repo));
    },
    sync: async (project: string, repo: string): Promise<ArchitectureSource> => {
      const row = rows.find((candidate) => same(candidate, project, repo));
      if (row === undefined) {
        throw new ApiError(404, { code: "SOURCE_NOT_FOUND" });
      }
      return { ...row };
    },
  };
}

test("a human adds an architecture source and the verified row appears", async () => {
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
      <QueryClientProvider client={queryClient}>
        <ArchitectureSourcesSection />
      </QueryClientProvider>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    expect(await section.findByText("No architecture sources yet.")).toBeDefined();

    fireEvent.change(section.getByLabelText("Source project"), { target: { value: "CORE" } });
    fireEvent.change(section.getByLabelText("Source repository"), {
      target: { value: "legion/arch" },
    });
    fireEvent.click(section.getByRole("button", { name: "Add source" }));
    await waitFor(() =>
      expect(putArchitectureSource).toHaveBeenLastCalledWith("CORE", "legion/arch", {
        branch: "main",
      })
    );
    await waitFor(() => expect(section.getByRole("cell", { name: "legion/arch" })).toBeDefined());
    expect(section.getByRole("cell", { name: "Access verified" })).toBeDefined();
  } finally {
    cleanup();
    queryClient.clear();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});

test("a failed architecture source access check shows the server's reason inline", async () => {
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
      <QueryClientProvider client={queryClient}>
        <ArchitectureSourcesSection />
      </QueryClientProvider>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    fireEvent.change(await section.findByLabelText("Source project"), {
      target: { value: "CORE" },
    });
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
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});

test("each architecture source of a project is its own row, refreshed and deleted on its own", async () => {
  const server = sourceServer([
    {
      ...architectureSource,
      components: 36,
      last_commit: "3be61df5aa001122334455667788990011223344",
      last_sync_at: "2026-09-10T00:00:00Z",
    },
    {
      ...architectureSource,
      components: 1,
      last_commit: "9f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6",
      last_error: 'component id "dispatch" is already a component of CORE from legion/arch',
      last_sync_at: "2026-09-10T00:00:00Z",
      repo: "acme/infra",
    },
  ]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockImplementation(
    server.list
  );
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" },
  ]);
  const putArchitectureSource = spyOn(api, "putArchitectureSource").mockImplementation(server.put);
  const syncArchitectureSource = spyOn(api, "syncArchitectureSource").mockImplementation(
    server.sync
  );
  const deleteArchitectureSource = spyOn(api, "deleteArchitectureSource").mockImplementation(
    server.remove
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ArchitectureSourcesSection />
      </QueryClientProvider>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    await section.findByRole("cell", { name: "legion/arch" });
    const rowOf = (repo: string) => section.getByRole("cell", { name: repo }).closest("tr");
    expect(section.getAllByRole("cell", { name: "CORE" })).toHaveLength(2);
    // Each row shows its own import: the healthy one its commit and component count, the
    // failed one its error and the commit and count it still supplies.
    expect(rowOf("legion/arch")?.textContent).toContain("Synced 3be61df5aa00 · 36 components");
    const failed = rowOf("acme/infra")?.textContent;
    expect(failed).toContain('component id "dispatch" is already a component of CORE');
    expect(failed).toContain("Previous model stays up at 9f1e2d3c4b5a · 1 component");
    expect(failed).not.toContain("1 components");

    fireEvent.click(
      section.getByRole("button", { name: "Refresh architecture source acme/infra for CORE" })
    );
    await waitFor(() => expect(syncArchitectureSource).toHaveBeenCalledWith("CORE", "acme/infra"));
    expect(syncArchitectureSource).toHaveBeenCalledTimes(1);
    await waitFor(() =>
      expect(
        section
          .getByRole("button", { name: "Delete architecture source legion/arch for CORE" })
          .hasAttribute("disabled")
      ).toBe(false)
    );

    fireEvent.click(
      section.getByRole("button", { name: "Delete architecture source legion/arch for CORE" })
    );
    await waitFor(() => expect(section.queryByRole("cell", { name: "legion/arch" })).toBeNull());
    expect(deleteArchitectureSource).toHaveBeenCalledTimes(1);
    expect(deleteArchitectureSource).toHaveBeenCalledWith("CORE", "legion/arch");
    expect(section.getByRole("cell", { name: "acme/infra" })).toBeDefined();

    // Saving a repository the project already has changes that source's branch: still one row.
    fireEvent.change(section.getByLabelText("Source project"), { target: { value: "CORE" } });
    fireEvent.change(section.getByLabelText("Source repository"), {
      target: { value: "acme/infra" },
    });
    fireEvent.change(section.getByLabelText("Branch"), { target: { value: "release" } });
    fireEvent.click(section.getByRole("button", { name: "Add source" }));
    await waitFor(() => expect(section.getByRole("cell", { name: "release" })).toBeDefined());
    expect(putArchitectureSource).toHaveBeenCalledWith("CORE", "acme/infra", {
      branch: "release",
    });
    expect(section.getAllByRole("cell", { name: "acme/infra" })).toHaveLength(1);
    expect(section.queryByRole("cell", { name: "main" })).toBeNull();
  } finally {
    cleanup();
    queryClient.clear();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
    syncArchitectureSource.mockRestore();
    deleteArchitectureSource.mockRestore();
  }
});
