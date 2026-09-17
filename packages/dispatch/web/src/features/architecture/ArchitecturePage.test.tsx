import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  waitForElementToBeRemoved,
  within,
} from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type {
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeIssue,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ArchitecturePage } from "./ArchitecturePage";

function component(
  overrides: Partial<ArchitectureTreeComponent> & { id: string }
): ArchitectureTreeComponent {
  return {
    depends_on: [],
    done: 0,
    external: false,
    issues: [],
    own_done: 0,
    own_total: 0,
    parent: null,
    paths: [],
    prose: "",
    title: overrides.id,
    total: 0,
    ...overrides,
  };
}

function issue(overrides: Partial<ArchitectureTreeIssue> & { key: string }): ArchitectureTreeIssue {
  return {
    attached: "direct",
    external_links: [],
    parent: null,
    priority: null,
    status: "todo",
    title: `Title of ${overrides.key}`,
    updated_at: "2026-09-17T00:00:00Z",
    ...overrides,
  };
}

const webIssues = [
  issue({ key: "CORE-1", status: "done", updated_at: "2026-09-10T00:00:00Z" }),
  issue({ key: "CORE-2", status: "in_progress", updated_at: "2026-09-12T00:00:00Z" }),
  issue({ attached: "inherited", key: "CORE-3", parent: "CORE-2", status: "todo" }),
];

function fixture(overrides: Partial<ArchitectureTree> = {}): ArchitectureTree {
  return {
    components: [
      component({
        done: 1,
        id: "legion",
        issues: webIssues.map((row) => ({ ...row, attached: "contained", via: "web" })),
        title: "Legion",
        total: 3,
      }),
      component({
        done: 1,
        id: "dispatch",
        issues: webIssues.map((row) => ({ ...row, attached: "contained", via: "web" })),
        parent: "legion",
        title: "Dispatch",
        total: 3,
      }),
      component({
        depends_on: ["server"],
        done: 1,
        id: "web",
        issues: webIssues,
        own_done: 1,
        own_total: 3,
        parent: "dispatch",
        paths: ["packages/dispatch"],
        prose: "The **SPA**.",
        title: "Dispatch web",
        total: 3,
      }),
      component({ id: "server", parent: "dispatch", title: "Dispatch server" }),
      component({ external: true, id: "github", parent: "legion", title: "GitHub" }),
      component({ done: 2, id: "docs", own_done: 2, own_total: 2, title: "Docs", total: 2 }),
    ],
    not_architectural: [
      {
        inherited_from: null,
        key: "CORE-8",
        reason: "hiring, not code",
        status: "todo",
        title: "Hire",
      },
      {
        inherited_from: "CORE-8",
        key: "CORE-9",
        reason: "hiring, not code",
        status: "todo",
        title: "Interview",
      },
    ],
    retired_links: [],
    source: {
      branch: "main",
      last_commit: "3be61df5be90",
      last_error: null,
      last_sync_at: new Date(Date.now() - 2 * 60_000).toISOString(),
      repo: "legion/legion",
    },
    totals: {
      components_without_work: 1,
      issues_done: 3,
      issues_total: 8,
      not_architectural: 2,
      retired_links: 0,
      unassigned: 1,
    },
    unassigned: [{ key: "CORE-7", status: "backlog", title: "Loose end" }],
    ...overrides,
  };
}

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.search}</output>;
}

function renderPane(tree: ArchitectureTree, search = "") {
  const getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(tree);
  const syncArchitectureSource = spyOn(api, "syncArchitectureSource");
  const patchIssue = spyOn(api, "patchIssue");
  const getIssue = spyOn(api, "getIssue");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[`/projects/CORE/architecture${search}`]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ArchitecturePage project="CORE" />
          <LocationProbe />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    getArchitecture,
    getIssue,
    patchIssue,
    queryClient,
    restore: () => {
      view.unmount();
      getArchitecture.mockRestore();
      getIssue.mockRestore();
      patchIssue.mockRestore();
      syncArchitectureSource.mockRestore();
      window.sessionStorage.clear();
    },
    syncArchitectureSource,
    view,
  };
}

const rowFor = (title: string) => screen.getByRole("article", { name: new RegExp(`^${title},`) });

test("the header reads the source and scope, and root rows carry bars, counts, and the external badge", async () => {
  const pane = renderPane(fixture());
  try {
    const source = await screen.findByTestId("architecture-source");
    expect(source.textContent).toContain("legion/legion/main");
    expect(source.textContent).toContain("at 3be61df5");
    expect(source.textContent).toContain("checked 2 minutes ago");
    expect(screen.getByTestId("architecture-scope").textContent).toBe(
      "3/8 issues done·1 component with no tracked work·1 unassigned·2 not architectural"
    );
    const rows = within(screen.getByRole("list", { name: "Components" })).getAllByRole("article");
    expect(rows.map((row) => row.getAttribute("aria-label"))).toEqual([
      "Legion, 1/3 issues done",
      "Docs, 2/2 issues done",
    ]);
    // Legion is partial: a fill inside the track, sized to done/total.
    const legion = rowFor("Legion");
    expect(legion.querySelector("[data-tone]")?.getAttribute("data-tone")).toBe("partial");
    expect((legion.querySelector('[data-testid="progress-fill"]') as HTMLElement).style.width).toBe(
      "33%"
    );
    // Docs is fully done with no descendants: a solid green bar and no fill element.
    const docs = rowFor("Docs");
    expect(docs.querySelector("[data-tone]")?.getAttribute("data-tone")).toBe("done");
    expect(docs.querySelector('[data-testid="progress-fill"]')).toBeNull();
    // Nothing is hatched while the model is fresh and the last import succeeded.
    expect(legion.classList.contains("border-dashed")).toBe(false);
  } finally {
    pane.restore();
  }
});

test("a chevron descends into a component's children with a breadcrumb back; external rows have no bar and no-work rows a dashed track", async () => {
  const pane = renderPane(fixture());
  try {
    await screen.findByRole("list", { name: "Components" });
    fireEvent.click(screen.getByRole("link", { name: "Open the components inside Legion" }));
    expect(screen.getByTestId("location").textContent).toBe("?component=legion");
    const rows = within(screen.getByRole("list", { name: "Components" })).getAllByRole("article");
    expect(rows.map((row) => row.getAttribute("aria-label"))).toEqual([
      "Dispatch, 1/3 issues done",
      "GitHub, external",
    ]);
    const github = rowFor("GitHub");
    expect(github.querySelector("[data-tone]")).toBeNull();
    expect(within(github).getByText("external")).toBeTruthy();
    const crumbs = screen.getByRole("navigation", { name: "Component level" });
    expect(crumbs.textContent).toBe("All components/Legion");

    fireEvent.click(screen.getByRole("link", { name: "Open the components inside Dispatch" }));
    expect(crumbs.textContent).toBe("All components/Legion/Dispatch");
    const server = rowFor("Dispatch server");
    expect(server.querySelector("[data-tone]")?.getAttribute("data-tone")).toBe("no-work");
    expect(within(server).getByText("No tracked work")).toBeTruthy();
    expect(server.getAttribute("aria-label")).toBe("Dispatch server, no tracked work");

    fireEvent.click(within(crumbs).getByRole("link", { name: "All components" }));
    expect(screen.getByTestId("location").textContent).toBe("");
    expect(rowFor("Legion")).toBeTruthy();
  } finally {
    pane.restore();
  }
});

test("j/k rove the rows, l/h descend and ascend, Enter opens a parent's children or a leaf's details, Escape leaves the row", async () => {
  const pane = renderPane(fixture());
  try {
    await screen.findByRole("list", { name: "Components" });
    fireEvent.keyDown(document.body, { key: "j" });
    expect(document.activeElement).toBe(rowFor("Legion"));
    fireEvent.keyDown(document.activeElement as Element, { key: "j" });
    expect(document.activeElement).toBe(rowFor("Docs"));
    fireEvent.keyDown(document.activeElement as Element, { key: "j" });
    expect(document.activeElement).toBe(rowFor("Docs"));
    fireEvent.keyDown(document.activeElement as Element, { key: "k" });
    expect(document.activeElement).toBe(rowFor("Legion"));

    fireEvent.keyDown(document.activeElement as Element, { key: "l" });
    await waitFor(() => expect(document.activeElement).toBe(rowFor("Dispatch")));
    expect(screen.getByTestId("location").textContent).toBe("?component=legion");
    fireEvent.keyDown(document.activeElement as Element, { key: "h" });
    await waitFor(() => expect(document.activeElement).toBe(rowFor("Legion")));
    expect(screen.getByTestId("location").textContent).toBe("");

    // Enter on a parent row goes into its children like `l`, focus following (not to <body>).
    fireEvent.keyDown(document.activeElement as Element, { key: "Enter" });
    await waitFor(() => expect(document.activeElement).toBe(rowFor("Dispatch")));
    expect(screen.getByTestId("location").textContent).toBe("?component=legion");
    fireEvent.keyDown(document.activeElement as Element, { key: "h" });
    await waitFor(() => expect(document.activeElement).toBe(rowFor("Legion")));

    fireEvent.keyDown(document.activeElement as Element, { key: "j" });
    fireEvent.keyDown(document.activeElement as Element, { key: "Enter" });
    expect(screen.getByTestId("location").textContent).toBe("?component=docs");
    expect(await screen.findByRole("heading", { level: 2, name: "Docs" })).toBeTruthy();

    (rowFor("Docs") as HTMLElement).focus();
    fireEvent.keyDown(document.activeElement as Element, { key: "Escape" });
    expect(document.activeElement).toBe(document.body);
  } finally {
    pane.restore();
  }
});

test("the details list open work first by status then activity, fold the done issues, mark attachments, and show code & definition", async () => {
  const pane = renderPane(fixture(), "?component=web");
  try {
    const details = await screen.findByTestId("component-details");
    expect(within(details).getByRole("heading", { level: 2, name: "Dispatch web" })).toBeTruthy();
    // A leaf's level is its parent: the siblings show above.
    expect(screen.getByRole("navigation", { name: "Component level" }).textContent).toBe(
      "All components/Legion/Dispatch"
    );
    const open = within(details).getByRole("list", { name: "Open work on Dispatch web" });
    expect(
      within(open)
        .getAllByRole("listitem")
        .map((row) => row.getAttribute("data-testid"))
    ).toEqual(["work-CORE-3", "work-CORE-2"]);
    expect(within(screen.getByTestId("work-CORE-3")).getByText("inherited")).toBeTruthy();
    expect(within(screen.getByTestId("work-CORE-2")).getByText("In progress")).toBeTruthy();
    expect(screen.queryByTestId("work-CORE-1")).toBeNull();
    fireEvent.click(within(details).getByRole("button", { name: "Show 1 done" }));
    expect(screen.getByTestId("work-CORE-1")).toBeTruthy();
    expect(
      within(details).getByRole("link", { name: "CORE-1 · Title of CORE-1" }).getAttribute("href")
    ).toBe("/issues/CORE-1");

    expect((await within(details).findByText("SPA")).tagName).toBe("STRONG");
    expect(within(details).getByText("packages/dispatch")).toBeTruthy();
    expect(
      within(details).getByRole("link", { name: "Dispatch server" }).getAttribute("href")
    ).toBe("/projects/CORE/architecture?component=server");

    // Under a parent, the contained marker names the descendant the issue's set names.
    fireEvent.click(screen.getByRole("link", { name: "Legion" }));
    await waitFor(() =>
      expect(screen.getByRole("heading", { level: 2, name: "Legion" })).toBeTruthy()
    );
    expect(within(screen.getByTestId("component-details")).getAllByText("in web")).toHaveLength(2);
  } finally {
    pane.restore();
  }
});

test("a work row's chevron expands the issue's children in place, dimming those counted elsewhere", async () => {
  const pane = renderPane(fixture(), "?component=web");
  try {
    pane.getIssue.mockResolvedValue({
      children: [
        {
          active_at: "2026-09-17T00:00:00Z",
          external_links: [],
          key: "CORE-3",
          status: "todo",
          subtree_done: 0,
          subtree_total: 1,
          title: "Title of CORE-3",
        },
        {
          active_at: "2026-09-17T00:00:00Z",
          external_links: [],
          key: "CORE-7",
          status: "backlog",
          subtree_done: 0,
          subtree_total: 1,
          title: "Loose end",
        },
        {
          active_at: "2026-09-17T00:00:00Z",
          external_links: [],
          key: "CORE-9",
          status: "todo",
          subtree_done: 0,
          subtree_total: 1,
          title: "Interview",
        },
      ],
    } as never);
    await screen.findByTestId("work-CORE-2");
    fireEvent.click(screen.getByRole("button", { name: "Expand children of CORE-2" }));
    const children = await screen.findByRole("list", { name: "Children of CORE-2" });
    expect(pane.getIssue).toHaveBeenCalledWith("CORE-2");
    const placements = within(children)
      .getAllByRole("listitem")
      .map((row) => row.getAttribute("data-placement"));
    expect(placements).toEqual(["here", "unassigned", "not-architectural"]);
    // The expansion survives a remount (Back from the issue page restores it).
    expect(window.sessionStorage.getItem("dispatch.architecture.expanded:CORE:web")).toBe(
      JSON.stringify(["CORE-2"])
    );
  } finally {
    pane.restore();
  }
});

test("never synced: the source line says so and an empty model shows the Refresh call to action; Refresh POSTs the sync", async () => {
  const pane = renderPane(
    fixture({
      components: [],
      source: {
        branch: "main",
        last_commit: null,
        last_error: null,
        last_sync_at: null,
        repo: "legion/legion",
      },
      totals: {
        components_without_work: 0,
        issues_done: 0,
        issues_total: 0,
        not_architectural: 0,
        retired_links: 0,
        unassigned: 0,
      },
      unassigned: [],
    })
  );
  try {
    const source = await screen.findByTestId("architecture-source");
    expect(source.textContent).toBe("legion/legion/main·never synced·Refresh");
    expect(screen.getByTestId("architecture-empty").textContent).toBe(
      "No model imported yet — Refresh reads .dispatch/architecture from legion/legion/main."
    );
    let settle: (() => void) | undefined;
    pane.syncArchitectureSource.mockImplementation(() => {
      const { promise, resolve } = Promise.withResolvers<never>();
      settle = () => resolve(undefined as never);
      return promise;
    });
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(pane.syncArchitectureSource).toHaveBeenCalledWith("CORE"));
    const refreshing = await screen.findByRole("button", { name: "Refreshing…" });
    expect(refreshing.hasAttribute("disabled")).toBe(true);
    settle?.();
    await screen.findByRole("button", { name: "Refresh" });
  } finally {
    pane.restore();
  }
});

test("a Refresh whose request itself fails says so beside the button until the next attempt", async () => {
  const pane = renderPane(fixture());
  try {
    const source = await screen.findByTestId("architecture-source");
    pane.syncArchitectureSource.mockRejectedValueOnce(
      new ApiError(500, { error: "GitHub is unreachable" })
    );
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await within(source).findByRole("alert")).toHaveProperty(
      "textContent",
      "GitHub is unreachable"
    );
    // The next attempt clears it.
    const { promise, resolve } = Promise.withResolvers<never>();
    pane.syncArchitectureSource.mockImplementationOnce(() => promise);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByRole("button", { name: "Refreshing…" });
    expect(within(source).queryByRole("alert")).toBeNull();
    resolve(undefined as never);
    await screen.findByRole("button", { name: "Refresh" });
  } finally {
    pane.restore();
  }
});

test("?component= naming a component the model no longer has says so above the roots instead of quietly showing them", async () => {
  const pane = renderPane(fixture(), "?component=legacy-ui");
  try {
    await screen.findByRole("list", { name: "Components" });
    expect(within(screen.getByTestId("architecture-pane")).getByRole("status").textContent).toBe(
      "Component legacy-ui is not in the current model"
    );
    expect(rowFor("Legion")).toBeTruthy();
    expect(screen.queryByTestId("component-details")).toBeNull();
  } finally {
    pane.restore();
  }
});

test("a failed import shows the banner with the last model's commit and hatches every row; a stale source hatches too", async () => {
  const failed = renderPane(
    fixture({
      source: {
        branch: "main",
        last_commit: "3be61df5be90",
        last_error: "duplicate component id api",
        last_sync_at: new Date(Date.now() - 60_000).toISOString(),
        repo: "legion/legion",
      },
    })
  );
  try {
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toBe(
      "Last import failed: duplicate component id api · showing the model from 3be61df5"
    );
    expect(rowFor("Legion").classList.contains("border-dashed")).toBe(true);
  } finally {
    failed.restore();
  }

  const stale = renderPane(
    fixture({
      source: {
        branch: "main",
        last_commit: "3be61df5be90",
        last_error: null,
        last_sync_at: new Date(Date.now() - 11 * 60_000).toISOString(),
        repo: "legion/legion",
      },
    })
  );
  try {
    const source = await screen.findByTestId("architecture-source");
    expect(source.textContent).toContain("checked 11 minutes ago (stale)");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(rowFor("Legion").classList.contains("border-dashed")).toBe(true);
  } finally {
    stale.restore();
  }
});

test("Unassigned: the picker PATCHes an explicit set, the row leaves the list at once and shows Saving until the refetch; a failure brings it back with the error", async () => {
  const pane = renderPane(fixture());
  try {
    await screen.findByRole("list", { name: "Components" });
    fireEvent.click(screen.getByRole("button", { name: "1 unassigned" }));
    expect(screen.getByTestId("location").textContent).toBe("?view=unassigned");
    const list = screen.getByTestId("unassigned-list");
    expect(within(list).getByRole("link", { name: "CORE-7 · Loose end" })).toBeTruthy();

    // The failure path first: the server refuses, the row stays and says why.
    pane.patchIssue.mockRejectedValueOnce(
      new ApiError(400, { code: "COMPONENTS_INPUT", error: "CORE has no component ghost" })
    );
    fireEvent.click(screen.getByRole("button", { name: "Attach CORE-7 to components" }));
    fireEvent.click(screen.getByRole("option", { name: "web · Dispatch web" }));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search components" }), {
      key: "Escape",
    });
    await waitFor(() =>
      expect(pane.patchIssue).toHaveBeenCalledWith("CORE-7", {
        components: { ids: ["web"], mode: "explicit" },
      })
    );
    expect(await within(list).findByRole("alert")).toHaveProperty(
      "textContent",
      "CORE has no component ghost"
    );
    expect(within(list).getByRole("link", { name: "CORE-7 · Loose end" })).toBeTruthy();
    // The optimistic move rolled back: the scope line still counts it.
    expect(screen.getByTestId("architecture-scope").textContent).toContain("1 unassigned");

    // The success path: the tree cache moves the issue at once; the row stays, saving, until
    // the refetch (which returns the moved tree) settles.
    const { promise, resolve } = Promise.withResolvers<never>();
    pane.patchIssue.mockImplementationOnce(() => promise);
    fireEvent.click(screen.getByRole("button", { name: "Attach CORE-7 to components" }));
    fireEvent.click(screen.getByRole("option", { name: "web · Dispatch web" }));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search components" }), {
      key: "Escape",
    });
    expect(await within(list).findByRole("status")).toHaveProperty("textContent", "Saving…");
    expect(screen.getByTestId("architecture-scope").textContent).toContain("0 unassigned");
    expect(within(list).getByRole("link", { name: "CORE-7 · Loose end" })).toBeTruthy();
    const moved = fixture({
      totals: { ...fixture().totals, unassigned: 0 },
      unassigned: [],
    });
    pane.getArchitecture.mockResolvedValue(moved);
    resolve({} as never);
    // Not `waitFor(() => expect(query()).toBeNull())`: a failing `toBeNull` on a happy-dom
    // element spends seconds inspecting it, starving the retry loop past the test timeout.
    await waitForElementToBeRemoved(() =>
      within(list).queryByRole("link", { name: "CORE-7 · Loose end" })
    );
    expect(
      within(list).getByText(
        "Every issue is attached to a component or declared not architectural."
      )
    ).toBeTruthy();
  } finally {
    pane.restore();
  }
});

test("a row whose write is in flight keeps its place in the list, and focus lands on the row at that index once the write settles", async () => {
  const pane = renderPane(
    fixture({
      totals: { ...fixture().totals, unassigned: 2 },
      unassigned: [
        { key: "CORE-7", status: "backlog", title: "Loose end" },
        { key: "CORE-10", status: "backlog", title: "Other loose end" },
      ],
    }),
    "?view=unassigned"
  );
  try {
    const list = await screen.findByTestId("unassigned-list");
    const keys = () =>
      within(list)
        .getAllByRole("listitem")
        .map((item) => item.getAttribute("data-testid"));
    expect(keys()).toEqual(["attachment-CORE-7", "attachment-CORE-10"]);

    const { promise, resolve } = Promise.withResolvers<never>();
    pane.patchIssue.mockImplementationOnce(() => promise);
    fireEvent.click(screen.getByRole("button", { name: "Attach CORE-7 to components" }));
    fireEvent.click(screen.getByRole("option", { name: "web · Dispatch web" }));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search components" }), {
      key: "Escape",
    });
    expect(await within(list).findByRole("status")).toHaveProperty("textContent", "Saving…");
    // The optimistic move took CORE-7 out of the tree's list; it still renders first here.
    expect(keys()).toEqual(["attachment-CORE-7", "attachment-CORE-10"]);

    pane.getArchitecture.mockResolvedValue(
      fixture({
        totals: { ...fixture().totals, unassigned: 1 },
        unassigned: [{ key: "CORE-10", status: "backlog", title: "Other loose end" }],
      })
    );
    resolve({} as never);
    await waitForElementToBeRemoved(() =>
      within(list).queryByRole("link", { name: "CORE-7 · Loose end" })
    );
    // The picker's trigger is gone, so focus moves to the row now first: CORE-10's link.
    await waitFor(() =>
      expect(document.activeElement).toBe(
        within(list).getByRole("link", { name: "CORE-10 · Other loose end" })
      )
    );
  } finally {
    pane.restore();
  }
});

test("Not architectural lists each reason, names the ancestor of an inherited row, and Reconsider PATCHes inherit on an issue's own row", async () => {
  const pane = renderPane(fixture(), "?view=none");
  try {
    const list = await screen.findByTestId("not-architectural-list");
    expect(within(list).getAllByText("hiring, not code")).toHaveLength(2);
    const inherited = within(list).getByTestId("attachment-CORE-9");
    expect(within(inherited).getByRole("link", { name: "CORE-8" }).getAttribute("href")).toBe(
      "/issues/CORE-8"
    );
    expect(within(inherited).queryByRole("button", { name: "Reconsider" })).toBeNull();
    pane.patchIssue.mockResolvedValue({} as never);
    fireEvent.click(
      within(within(list).getByTestId("attachment-CORE-8")).getByRole("button", {
        name: "Reconsider",
      })
    );
    await waitFor(() =>
      expect(pane.patchIssue).toHaveBeenCalledWith("CORE-8", { components: { mode: "inherit" } })
    );
  } finally {
    pane.restore();
  }
});

test("the Not architectural… form on an Unassigned row PATCHes none with the reason", async () => {
  const pane = renderPane(fixture(), "?view=unassigned");
  try {
    const list = await screen.findByTestId("unassigned-list");
    pane.patchIssue.mockResolvedValue({} as never);
    fireEvent.click(within(list).getByRole("button", { name: "Not architectural…" }));
    fireEvent.change(within(list).getByLabelText("Why CORE-7 is not architectural"), {
      target: { value: "  a hiring task  " },
    });
    fireEvent.click(within(list).getByRole("button", { name: "Save reason" }));
    await waitFor(() =>
      expect(pane.patchIssue).toHaveBeenCalledWith("CORE-7", {
        components: { mode: "none", reason: "a hiring task" },
      })
    );
  } finally {
    pane.restore();
  }
});

test("Retired: the re-attach picker starts from the issue's surviving live components, so the save adds the pick and keeps them", async () => {
  const pane = renderPane(
    fixture({
      retired_links: [{ ids: ["legacy-ui"], key: "CORE-2", status: "in_progress", title: "Two" }],
      totals: { ...fixture().totals, retired_links: 1 },
    }),
    "?view=retired"
  );
  try {
    const list = await screen.findByTestId("retired-list");
    expect(within(list).getByText("retired: legacy-ui")).toBeTruthy();
    pane.patchIssue.mockResolvedValue({} as never);
    fireEvent.click(within(list).getByRole("button", { name: "Re-attach CORE-2 to components" }));
    // CORE-2 is `direct` on web (and only `contained` on web's ancestors): web is pre-selected.
    expect(
      screen.getByRole("option", { name: "web · Dispatch web" }).getAttribute("aria-selected")
    ).toBe("true");
    expect(
      screen.getByRole("option", { name: "legion · Legion" }).getAttribute("aria-selected")
    ).toBe("false");
    fireEvent.click(screen.getByRole("option", { name: "server · Dispatch server" }));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search components" }), {
      key: "Escape",
    });
    await waitFor(() =>
      expect(pane.patchIssue).toHaveBeenCalledWith("CORE-2", {
        components: { ids: ["web", "server"], mode: "explicit" },
      })
    );
  } finally {
    pane.restore();
  }
});

test("the no-tracked-work link filters the tree to those components, wherever they sit", async () => {
  const pane = renderPane(fixture());
  try {
    await screen.findByRole("list", { name: "Components" });
    fireEvent.click(screen.getByRole("button", { name: "1 component with no tracked work" }));
    const rows = within(screen.getByRole("list", { name: "Components" })).getAllByRole("article");
    expect(rows.map((row) => row.getAttribute("aria-label"))).toEqual([
      "Dispatch server, no tracked work",
    ]);
    expect(within(rows[0] as HTMLElement).getByText("Legion / Dispatch")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear filter" }));
    expect(rowFor("Legion")).toBeTruthy();
  } finally {
    pane.restore();
  }
});
