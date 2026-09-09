import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
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
