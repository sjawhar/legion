import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api, type ListEventsOptions } from "../../api/client";
import type { Issue } from "../../api/types";
import { IssuePage } from "./IssuePage";

const issue: Issue = {
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

function issueWithExternalLink(url: string): Issue {
  return { ...issue, external_links: [{ url }] };
}

function stubIssuePage(nextIssue: Issue): () => void {
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

function renderIssuePage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
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
