import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type { IssueDetails } from "../api/types";
import { AuthGate } from "../app";

const issue: IssueDetails = {
  artifacts: [
    {
      created_at: "2026-09-10T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      project: "CORE",
      kind: "doc",
      name: "Spec",
      primary: true,
      slug: "spec",
      versions: [],
    },
  ],
  children: [],
  closed_at: null,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 0,
  number: 1,
  open_asks: [],
  parent: null,
  primary_artifact_id: "artifact-1",
  project: "CORE",
  route: null,
  status: "todo",
  title: "Responsive shell",
  updated_at: "2026-09-10T00:00:00Z",
};

const originalApi = {
  getInbox: api.getInbox,
  getIssue: api.getIssue,
  getMyState: api.getMyState,
  listComments: api.listComments,
  listIssueAsks: api.listIssueAsks,
  listIssues: api.listIssues,
  listProjects: api.listProjects,
  whoAmI: api.whoAmI,
};
const originalEventSource = globalThis.EventSource;
const originalMatchMedia = window.matchMedia;

class TestEventSource {
  addEventListener(): void {}

  close(): void {}
}

function setViewport(width: number): void {
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query === "(max-width: 1279px)" && width <= 1279,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
}

function renderShellAt(width: number): void {
  setViewport(width);
  api.whoAmI = async () => ({ kind: "user", login: "alice" });
  api.listIssues = async () => [
    {
      key: issue.key,
      open_asks: 0,
      last_seq: 0,
      parent: issue.parent,
      status: issue.status,
      title: issue.title,
      updated_at: issue.updated_at,
    },
  ];
  api.getMyState = async () => ({ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false } });
  api.getIssue = async () => issue;
  api.getInbox = async () => [];
  api.listProjects = async () => [
    { created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 },
  ];
  api.listIssueAsks = async () => [];
  api.listComments = async () => [];
  globalThis.EventSource = TestEventSource as unknown as typeof EventSource;

  render(
    <QueryClientProvider
      client={
        new QueryClient({
          defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
        })
      }
    >
      <MemoryRouter initialEntries={["/issues/CORE-1/children"]}>
        <AuthGate />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

afterEach(() => {
  cleanup();
  Object.assign(api, originalApi);
  globalThis.EventSource = originalEventSource;
  window.matchMedia = originalMatchMedia;
});

test("tablet uses the drawer and bottom-sheet controls instead of hidden desktop landmarks", async () => {
  renderShellAt(800);

  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Open navigation" })).not.toBeNull()
  );
  expect(screen.queryByRole("button", { name: /Open review panel/ })).not.toBeNull();
  expect(screen.queryByRole("navigation", { name: "Issues" })).toBeNull();
  expect(screen.queryByRole("complementary", { name: "Review margin" })).toBeNull();
});

test("desktop exposes the persistent sidebar navigation and review margin", async () => {
  renderShellAt(1280);

  await waitFor(() =>
    expect(screen.queryByRole("navigation", { name: "Navigation" })).not.toBeNull()
  );
  expect(screen.queryByRole("complementary", { name: "Review margin" })).not.toBeNull();
  expect(screen.queryByRole("button", { name: "Open navigation" })).toBeNull();
  expect(screen.queryByRole("button", { name: /Open review panel/ })).toBeNull();
});
