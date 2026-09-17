import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { ArchitectureSource, ArchitectureTree, IssueComponents } from "../../api/types";
import { IssueComponentsLine } from "./IssueComponentsLine";

const tree: ArchitectureTree = {
  components: [
    {
      depends_on: [],
      done: 0,
      external: false,
      id: "dispatch-server",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      title: "Dispatch server",
      total: 0,
    },
    {
      depends_on: [],
      done: 0,
      external: true,
      id: "github",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      title: "GitHub",
      total: 0,
    },
  ],
  not_architectural: [],
  retired_links: [],
  source: {
    branch: "main",
    last_commit: "abc",
    last_error: null,
    last_sync_at: "2026-09-17T00:00:00Z",
    repo: "legion/legion",
  },
  totals: {
    components_without_work: 1,
    issues_done: 0,
    issues_total: 0,
    not_architectural: 0,
    retired_links: 0,
    unassigned: 0,
  },
  unassigned: [],
};

const source: ArchitectureSource = {
  branch: "main",
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: "3be61df5be90",
  last_error: null,
  last_sync_at: "2026-09-17T00:00:00Z",
  project: "CORE",
  repo: "legion/legion",
};

// Queries are scoped to this render's container: bun runs every test file in
// one document, and a sibling file's un-unmounted render would otherwise leak
// its links into a `screen`-wide role query.
/** `architecture` is the project's tree, or `undefined` for a project without a source (the
 *  source lookup answers 404 SOURCE_NOT_FOUND). */
function renderLine(components: IssueComponents, architecture: ArchitectureTree | undefined) {
  const getArchitectureSource = spyOn(api, "getArchitectureSource");
  const getArchitecture = spyOn(api, "getArchitecture");
  if (architecture === undefined) {
    getArchitectureSource.mockRejectedValue(new ApiError(404, { code: "SOURCE_NOT_FOUND" }));
    getArchitecture.mockRejectedValue(new ApiError(404, { code: "SOURCE_NOT_FOUND" }));
  } else {
    getArchitectureSource.mockResolvedValue(source);
    getArchitecture.mockResolvedValue(architecture);
  }
  const patchIssue = spyOn(api, "patchIssue");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <IssueComponentsLine
          issue={{ components, key: "CORE-7", project: "CORE", status: "todo", title: "Ship" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    ...view,
    getArchitecture,
    getArchitectureSource,
    patchIssue,
    q: within(view.container),
    unmountAll: () => {
      view.unmount();
      getArchitecture.mockRestore();
      getArchitectureSource.mockRestore();
      patchIssue.mockRestore();
    },
  };
}

test("IssueComponentsLine reads Not attached, and stays read-only without an architecture source", async () => {
  const view = renderLine(
    { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    undefined
  );
  try {
    expect(view.q.getByTestId("issue-components").textContent).toBe("Components:Not attached");
    expect(view.q.queryByRole("link")).toBeNull();
    await waitFor(() => expect(view.getArchitectureSource).toHaveBeenCalledTimes(1));
    expect(view.q.queryByRole("button", { name: "Edit components of CORE-7" })).toBeNull();
    // No source: the tree is never asked for.
    expect(view.getArchitecture).not.toHaveBeenCalled();
  } finally {
    view.unmountAll();
  }
});

test("IssueComponentsLine lists an explicit set as chips, marks retired ids, and links the ancestor it came from", () => {
  const view = renderLine(
    {
      mode: "explicit",
      ids: ["dispatch-server", "web"],
      unknown: ["legacy-ui"],
      reason: null,
      inherited_from: "CORE-1",
    },
    undefined
  );
  try {
    view.q.getByText("dispatch-server");
    view.q.getByText("web");
    const retired = view.q.getByText("retired: legacy-ui");
    expect(retired.getAttribute("title")).toBe(
      "legacy-ui is no longer in the project's architecture model"
    );
    const ancestor = view.q.getByRole("link", { name: "CORE-1" }) as HTMLAnchorElement;
    expect(ancestor.getAttribute("href")).toBe("/issues/CORE-1");
    expect(view.q.getByTestId("issue-components").textContent).toContain("inherited from CORE-1");
  } finally {
    view.unmountAll();
  }
});

test("IssueComponentsLine shows a none attachment with its reason", () => {
  const view = renderLine(
    { mode: "none", ids: [], unknown: [], reason: "hiring, not code", inherited_from: null },
    undefined
  );
  try {
    expect(view.q.getByTestId("issue-components").textContent).toBe(
      "Components:None — hiring, not code"
    );
    expect(view.q.queryByRole("link")).toBeNull();
  } finally {
    view.unmountAll();
  }
});

test("with an architecture source the line offers the picker, fetching the tree only once it opens, and PATCHes an explicit set", async () => {
  const view = renderLine(
    { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    tree
  );
  try {
    view.patchIssue.mockResolvedValue({} as never);
    const trigger = await view.q.findByRole("button", { name: "Edit components of CORE-7" });
    expect(trigger.textContent).toBe("Set components");
    // The header decides editability from the source lookup alone: the tree (refetched on
    // every issue event in the project) waits for the picker.
    expect(view.getArchitecture).not.toHaveBeenCalled();
    fireEvent.click(trigger);
    await waitFor(() => expect(view.getArchitecture).toHaveBeenCalledTimes(1));
    const option = await screen.findByRole("option", { name: "dispatch-server · Dispatch server" });
    const listbox = screen.getByRole("listbox", { name: "Components options" });
    // The external component is not offered: the server refuses to attach it.
    expect(
      within(listbox)
        .getAllByRole("option")
        .map((row) => row.textContent)
    ).toEqual(["dispatch-server · Dispatch server"]);
    fireEvent.click(option);
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search components" }), {
      key: "Escape",
    });
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    expect(view.patchIssue).toHaveBeenCalledWith("CORE-7", {
      components: { ids: ["dispatch-server"], mode: "explicit" },
    });
  } finally {
    view.unmountAll();
  }
});
