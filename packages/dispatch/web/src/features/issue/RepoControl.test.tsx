import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import type { Issue, IssueDetails, IssueSummary, RepoProject } from "../../api/types";
import { RepoControl } from "./RepoControl";

const issue: Issue = {
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  closed_at: null,
  created_at: "2026-10-01T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  blocked_by: [],
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: "acme/widgets",
  route: null,
  status: "todo",
  title: "Repository work",
  updated_at: "2026-10-01T00:00:00Z",
};
const summary: IssueSummary = {
  assignee: null,
  blocked_by: [],
  claim: null,
  components: issue.components,
  key: issue.key,
  labels: [],
  last_seq: 1,
  open_asks: 0,
  parent: null,
  primary_artifact_id: "artifact-1",
  priority: null,
  progress: { tasks: null, children: null },
  rank: "U",
  repo: null,
  resolved_repo: "acme/widgets",
  route: null,
  route_holder: null,
  route_status: null,
  status: "todo",
  title: issue.title,
  updated_at: issue.updated_at,
};
const mappings: RepoProject[] = [
  {
    repo: "acme/widgets",
    project: "CORE",
    default: true,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-10-01T00:00:00Z",
  },
  {
    repo: "acme/gadgets",
    project: "CORE",
    default: false,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-10-01T00:00:00Z",
  },
  {
    repo: "other/thing",
    project: "OTHER",
    default: true,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-10-01T00:00:00Z",
  },
];

const detailKey = ["issue", issue.key] as const;
const listKey = ["issues", "project", issue.project] as const;

function CachedControl(): ReactNode {
  const cached = useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) {
    return null;
  }
  return (
    <RepoControl
      issueKey={cached.data.key}
      project={cached.data.project}
      repo={cached.data.repo}
      resolvedRepo={cached.data.resolved_repo}
    />
  );
}

function renderControl(
  own: { repo: string | null; resolvedRepo: string | null },
  mapped: RepoProject[] = mappings,
  { failFirstRead = false }: { failFirstRead?: boolean } = {}
) {
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue(mapped);
  if (failFirstRead) {
    listRepoProjects.mockRejectedValueOnce(new Error("the network dropped the read"));
  }
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData<IssueDetails>(detailKey, {
    ...issue,
    repo: own.repo,
    resolved_repo: own.resolvedRepo,
    route_status: null,
    route_holder: null,
    artifacts: [],
    children: [],
    open_asks: [],
    progress: { tasks: null, children: null },
    referenced_by_count: 0,
  });
  queryClient.setQueryData(listKey, [
    { ...summary, repo: own.repo, resolved_repo: own.resolvedRepo },
  ]);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <CachedControl />
    </QueryClientProvider>
  );
  return {
    listRepoProjects,
    queryClient,
    unmount: () => {
      view.unmount();
      listRepoProjects.mockRestore();
    },
  };
}

function select(): HTMLSelectElement {
  return screen.getByRole("combobox", { name: "Repository of CORE-1" }) as HTMLSelectElement;
}

/** The visible badge; the select's description repeats its text in a span of its own. */
const badge = 'span[aria-hidden="true"]';

test("RepoControl lists the project's mapped repositories, shows the resolved one, and patches repo", async () => {
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue({
    ...issue,
    repo: "acme/gadgets",
    resolved_repo: "acme/gadgets",
    last_seq: 2,
  });
  const { listRepoProjects, queryClient, unmount } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  try {
    // The badge names the repository the issue resolves to; no mapping is read until the reader
    // reaches for the control.
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
    expect(listRepoProjects).not.toHaveBeenCalled();
    fireEvent.focus(select());
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalled());
    // Inherit first, then CORE's mappings sorted; OTHER's absent.
    await waitFor(() =>
      expect(Array.from(select().options).map((option) => option.value)).toEqual([
        "",
        "acme/gadgets",
        "acme/widgets",
      ])
    );
    expect(select().value).toBe("");
    fireEvent.change(select(), { target: { value: "acme/gadgets" } });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/gadgets" })
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(select().value).toBe("acme/gadgets");
    expect(screen.getByText("acme/gadgets", { selector: badge })).toBeTruthy();
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)?.[0]?.repo).toBe("acme/gadgets");
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("RepoControl sends null for Inherit", async () => {
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
  });
  const { unmount } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    // The issue's own repository is selectable before the mapping list has loaded.
    expect(select().value).toBe("acme/gadgets");
    fireEvent.change(select(), { target: { value: "" } });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }));
    await waitFor(() => expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy());
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// The badge is decoration for sighted readers (aria-hidden); the select names the repository the
// issue resolves to in its description, so a screen reader hears the target as well as Inherit.
test("RepoControl describes the repository the issue resolves to", async () => {
  const description = () =>
    document.getElementById(select().getAttribute("aria-describedby") ?? "")?.textContent ?? null;
  for (const [own, want] of [
    [
      { repo: null, resolvedRepo: "acme/widgets" },
      "acme/widgets, inherited or the project's default",
    ],
    [{ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" }, "acme/gadgets"],
    [{ repo: null, resolvedRepo: null }, "No repository"],
  ] as const) {
    const { unmount } = renderControl({ ...own });
    try {
      await screen.findByRole("combobox", { name: "Repository of CORE-1" });
      expect(description()).toBe(want);
    } finally {
      unmount();
    }
  }
});

// With nothing resolved the control cannot tell from the issue whether its project maps any
// repository, so it reads the mappings at once: one is there to pick, so the badge says so.
test("RepoControl says No repository when nothing resolves in a project that maps one", async () => {
  const { listRepoProjects, unmount } = renderControl({ repo: null, resolvedRepo: null });
  try {
    expect(await screen.findByText("No repository", { selector: badge })).toBeTruthy();
    expect(screen.getByText("Repository:")).toBeTruthy();
    expect(listRepoProjects).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
  }
});

// A project that maps no repository offers nothing to pick, so the details rail carries no
// repository item at all.
test("RepoControl shows nothing for an issue whose project maps no repository", async () => {
  const others = mappings.filter((mapping) => mapping.project !== "CORE");
  const { queryClient, unmount } = renderControl({ repo: null, resolvedRepo: null }, others);
  try {
    // The mappings are read at once; once they are in and React has rendered them, the control
    // has decided.
    await waitFor(() =>
      expect(queryClient.getQueryData<RepoProject[]>(["repo-projects"])).toEqual(others)
    );
    await act(async () => {});
    expect(screen.queryByRole("combobox", { name: "Repository of CORE-1" })).toBeNull();
    expect(screen.queryByText("Repository:")).toBeNull();
  } finally {
    unmount();
  }
});

// A failed mappings read is not an empty one: with nothing resolved the control cannot tell
// whether the project maps a repository, so it says the read failed and offers it again rather
// than hiding the item.
test("RepoControl shows a failed mappings read with a retry instead of hiding the item", async () => {
  const { listRepoProjects, unmount } = renderControl(
    { repo: null, resolvedRepo: null },
    mappings,
    { failFirstRead: true }
  );
  try {
    expect(await screen.findByText("Could not load the repositories mapped to CORE.")).toBeTruthy();
    expect(screen.getByText("Repository:")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(select().options.length).toBe(3));
    expect(screen.queryByText("Could not load the repositories mapped to CORE.")).toBeNull();
    expect(screen.getByText("No repository", { selector: badge })).toBeTruthy();
  } finally {
    unmount();
  }
});

// With a repository resolved the picker still shows it, and a failed read says so beside it.
test("RepoControl shows a failed mappings read beside a resolved repository", async () => {
  const { unmount } = renderControl({ repo: null, resolvedRepo: "acme/widgets" }, mappings, {
    failFirstRead: true,
  });
  try {
    fireEvent.focus(select());
    expect(await screen.findByText("Could not load the repositories mapped to CORE.")).toBeTruthy();
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    unmount();
  }
});

test("RepoControl rolls back and shows the server's refusal", async () => {
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(
    new ApiError(400, {
      code: "REPO_NOT_MAPPED",
      error: "acme/gadgets is not mapped to project CORE (Settings → Repositories → Projects)",
    })
  );
  const { unmount } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    fireEvent.focus(select());
    await waitFor(() => expect(select().options.length).toBe(3));
    fireEvent.change(select(), { target: { value: "acme/gadgets" } });
    expect(
      await screen.findByText(
        "acme/gadgets is not mapped to project CORE (Settings → Repositories → Projects)"
      )
    ).toBeTruthy();
    expect(select().value).toBe("");
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});
