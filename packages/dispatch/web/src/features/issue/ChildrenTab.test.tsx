import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { ChildrenTab } from "./ChildrenTab";

const issue: IssueDetails = {
  artifacts: [],
  children: [
    {
      key: "CORE-2",
      title: "Branch",
      status: "in_progress",
      subtree_done: 1,
      subtree_total: 2,
      active_at: "2026-09-15T12:00:00Z",
      external_links: [{ url: "https://github.com/owner/repo/pull/12", kind: "github_pr" }],
    },
    {
      key: "CORE-3",
      title: "Leaf",
      status: "todo",
      subtree_done: 0,
      subtree_total: 1,
      active_at: "2026-09-14T08:00:00Z",
      external_links: [],
    },
  ],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 4,
  number: 1,
  open_asks: [],
  parent: null,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  primary_artifact_id: "artifact-1",
  project: "CORE",
  referenced_by_count: 0,
  route: null,
  status: "todo",
  priority: null,
  rank: "U",
  title: "Root",
  updated_at: "2026-09-09T00:00:00Z",
};

function renderChildren() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ChildrenTab issue={issue} />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("ChildrenTab shows each child's subtree progress, activity, and PR link", async () => {
  // No GitHub credentials: GitHubLink must still render the link itself.
  const githubRest = spyOn(api, "githubRest").mockRejectedValue(
    new ApiError(403, { code: "GITHUB_TOKEN_UNAVAILABLE", error: "no token" })
  );
  const view = renderChildren();
  try {
    screen.getByText("1/2 done");
    screen.getByText("0/1 done");

    const times = view.container.querySelectorAll("time");
    expect([...times].map((time) => time.getAttribute("datetime"))).toEqual([
      "2026-09-15T12:00:00Z",
      "2026-09-14T08:00:00Z",
    ]);

    const pullRequest = (await screen.findByRole("link", { name: /#12/ })) as HTMLAnchorElement;
    expect(pullRequest.getAttribute("href")).toBe("https://github.com/owner/repo/pull/12");

    const branch = screen.getByRole("link", { name: "CORE-2 · Branch" }) as HTMLAnchorElement;
    expect(branch.getAttribute("href")).toBe("/issues/CORE-2");
  } finally {
    view.unmount();
    githubRest.mockRestore();
  }
});
