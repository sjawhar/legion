import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";

import { api, type ListEventsOptions } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { IssuePage } from "./IssuePage";

const issue: IssueDetails = {
  artifacts: [
    {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "doc",
      name: "spec.md",
      primary: true,
      slug: "spec",
      versions: [],
    },
  ],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  open_asks: [],
  parent: null,
  primary_artifact_id: "artifact-1",
  project: "CORE",
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};

function issueWithExternalLink(url: string): IssueDetails {
  return { ...issue, external_links: [{ url }] };
}

function stubIssuePage(nextIssue: IssueDetails): () => void {
  const originalGetIssue = api.getIssue;
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetInbox = api.getInbox;
  const originalGetMyState = api.getMyState;
  api.getIssue = async () => nextIssue;
  api.getInbox = async () => [];
  api.getMyState = async () => ({ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false } });
  api.getIssueEvents = async () => [];
  return () => {
    api.getIssue = originalGetIssue;
    api.getIssueEvents = originalGetIssueEvents;
    api.getInbox = originalGetInbox;
    api.getMyState = originalGetMyState;
  };
}

function renderIssuePage(path = "/issues/CORE-1") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/issues/:key/*" element={<IssuePage user={{ login: "alice" }} />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("IssuePage reads newest events when looking for active sessions", async () => {
  const originalGetIssue = api.getIssue;
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetInbox = api.getInbox;
  const originalGetMyState = api.getMyState;
  const calls: ListEventsOptions[] = [];
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });

  try {
    api.getIssue = async () => issue;
    api.getInbox = async () => [];
    api.getMyState = async () => ({ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false } });
    api.getIssueEvents = async (_key, options = {}) => {
      calls.push(options);
      return [];
    };

    const view = render(
      <MemoryRouter initialEntries={["/issues/CORE-1"]}>
        <QueryClientProvider client={queryClient}>
          <Routes>
            <Route path="/issues/:key/*" element={<IssuePage user={{ login: "alice" }} />} />
          </Routes>
        </QueryClientProvider>
      </MemoryRouter>
    );

    await waitFor(() => expect(calls.length).toBeGreaterThanOrEqual(2));
    expect(calls.every((options) => options.limit === 200 && options.order === "desc")).toBe(true);
    view.unmount();
  } finally {
    api.getIssue = originalGetIssue;
    api.getIssueEvents = originalGetIssueEvents;
    api.getInbox = originalGetInbox;
    api.getMyState = originalGetMyState;
  }
});

test("a failed title save reverts the heading to the server value", async () => {
  const restore = stubIssuePage(issue);
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(new Error("offline"));
  const view = renderIssuePage();

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    fireEvent.click(screen.getByRole("heading", { level: 1, name: issue.title }));
    const input = screen.getByLabelText("Issue title");
    fireEvent.change(input, { target: { value: "A title that will not save" } });
    fireEvent.blur(input);

    await waitFor(() => expect(patchIssue).toHaveBeenCalled());
    await screen.findByRole("heading", { level: 1, name: issue.title });
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

for (const fixture of [
  {
    checkRuns: [{ conclusion: "success", status: "completed" }],
    checks: "success",
  },
  {
    checkRuns: [{ conclusion: "failure", status: "completed" }],
    checks: "failure",
  },
  {
    checkRuns: [{ conclusion: null, status: "in_progress" }],
    checks: "pending",
  },
]) {
  test(`IssuePage renders a merged pull request with ${fixture.checks} checks`, async () => {
    const restore = stubIssuePage(
      issueWithExternalLink("https://github.com/owner/repository/pull/7")
    );
    const githubRest = spyOn(api, "githubRest").mockImplementation(async (path) => {
      if (path === "repos/owner/repository/pulls/7") {
        return new Response(
          JSON.stringify({
            head: { sha: "abcdef" },
            merged: true,
            state: "closed",
            title: "Merge native Dispatch",
          }),
          { headers: { "Content-Type": "application/json" } }
        );
      }
      if (path === "repos/owner/repository/commits/abcdef/check-runs") {
        return new Response(JSON.stringify({ check_runs: fixture.checkRuns }), {
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(JSON.stringify({ state: "closed", title: "Wrong GitHub endpoint" }), {
        headers: { "Content-Type": "application/json" },
      });
    });
    const view = renderIssuePage();

    try {
      await screen.findByText("Merge native Dispatch");
      expect(screen.getByText("merged")).toBeDefined();
      expect(screen.getByText(`checks: ${fixture.checks}`)).toBeDefined();
    } finally {
      view.unmount();
      githubRest.mockRestore();
      restore();
    }
  });
}

test("IssuePage continues to unfurl GitHub issues through the issues endpoint", async () => {
  const restore = stubIssuePage(
    issueWithExternalLink("https://github.com/owner/repository/issues/9")
  );
  const githubRest = spyOn(api, "githubRest").mockResolvedValue(
    new Response(JSON.stringify({ state: "open", title: "Issue remains an issue" }), {
      headers: { "Content-Type": "application/json" },
    })
  );
  const view = renderIssuePage();

  try {
    await screen.findByText("Issue remains an issue");
    expect(screen.getByText("open")).toBeDefined();
    expect(githubRest).toHaveBeenCalledWith("repos/owner/repository/issues/9");
  } finally {
    view.unmount();
    githubRest.mockRestore();
    restore();
  }
});

test("IssuePage defaults the bare issue route to the Log tab", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1");

  try {
    await screen.findByRole("tab", { name: "Log", selected: true });
    expect(screen.getByRole("tab", { name: "Spec", selected: false })).toBeDefined();
    expect(screen.getByRole("tab", { name: "Children", selected: false })).toBeDefined();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage renders a not-found view for an unrecognized tab suffix without fetching the issue", async () => {
  const getIssueSpy = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getMyStateSpy = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false },
  });
  const view = renderIssuePage("/issues/CORE-1/not-a-tab");

  try {
    await screen.findByRole("heading", { name: "Page not found" });
    expect(screen.getByRole("link", { name: "Back to inbox" })).toBeDefined();
    expect(screen.queryByRole("tablist", { name: "Issue detail" })).toBeNull();
    expect(document.title).toBe("Not found · Dispatch");
    expect(getIssueSpy).not.toHaveBeenCalled();
    expect(getMyStateSpy).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getIssueSpy.mockRestore();
    getMyStateSpy.mockRestore();
  }
});

test("IssuePage remounts when switching issues, discarding unsaved local state", async () => {
  const secondIssue: IssueDetails = {
    ...issue,
    artifacts: [
      {
        created_at: "2026-09-09T00:00:00Z",
        created_by: { id: "alice", kind: "user" },
        id: "artifact-2",
        issue_key: "CORE-2",
        kind: "doc",
        name: "spec.md",
        primary: true,
        slug: "spec",
        versions: [],
      },
    ],
    key: "CORE-2",
    primary_artifact_id: "artifact-2",
    route: "role:second-issue-route",
    title: "Second issue",
  };
  const originalGetIssue = api.getIssue;
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetInbox = api.getInbox;
  const originalGetMyState = api.getMyState;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // Pre-warm CORE-2 so switching to it is an instant cache hit with no
  // pending/loading transition — the scenario where a stale-state leak would
  // otherwise go unnoticed by a naive loading-gate remount.
  queryClient.setQueryData(["issue", "CORE-2"], secondIssue);

  try {
    api.getIssue = async (key: string) => (key === "CORE-2" ? secondIssue : issue);
    api.getInbox = async () => [];
    api.getMyState = async () => ({
      "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false },
      "CORE-2": { dismissed: [], last_read_seq: 0, pinned: false },
    });
    api.getIssueEvents = async () => [];

    const view = render(
      <MemoryRouter initialEntries={["/issues/CORE-1"]}>
        <QueryClientProvider client={queryClient}>
          <Link to="/issues/CORE-2">Go to CORE-2</Link>
          <Routes>
            <Route path="/issues/:key/*" element={<IssuePage user={{ login: "alice" }} />} />
          </Routes>
        </QueryClientProvider>
      </MemoryRouter>
    );

    try {
      fireEvent.click(await screen.findByText("No route — messages stay on the issue"));
      const routeInput = await screen.findByLabelText("Route");
      fireEvent.change(routeInput, { target: { value: "role:not-saved-draft" } });
      await screen.findByDisplayValue("role:not-saved-draft");

      fireEvent.click(screen.getByRole("link", { name: "Go to CORE-2" }));

      await screen.findByRole("heading", { level: 1, name: "Second issue" });
      // The route field marks itself dirty on edit and, absent a remount,
      // skips resyncing to the newly loaded issue's own route — leaking
      // CORE-1's unsaved draft onto CORE-2's page instead of showing CORE-2's
      // own route.
      expect(screen.queryByDisplayValue("role:not-saved-draft")).toBeNull();
      expect(await screen.findByText("Messages also reach role:second-issue-route")).toBeDefined();
    } finally {
      view.unmount();
    }
  } finally {
    api.getIssue = originalGetIssue;
    api.getIssueEvents = originalGetIssueEvents;
    api.getInbox = originalGetInbox;
    api.getMyState = originalGetMyState;
  }
});
