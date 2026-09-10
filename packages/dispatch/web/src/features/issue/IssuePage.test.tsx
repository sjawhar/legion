import { afterAll, expect, spyOn, test } from "bun:test";
import { HocuspocusProvider } from "@hocuspocus/provider";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Link, MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { api, type ListEventsOptions } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { IssuePage } from "./IssuePage";

class WebSocketStub {
  binaryType = "arraybuffer";
  identifier = 0;
  readyState = 0;

  addEventListener(..._args: unknown[]): void {}

  close(): void {
    this.readyState = 3;
  }

  removeEventListener(..._args: unknown[]): void {}

  send(..._args: unknown[]): void {}
}

const originalWebSocket = globalThis.WebSocket;
globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;

afterAll(() => {
  globalThis.WebSocket = originalWebSocket;
});

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

function CurrentRoute() {
  const location = useLocation();
  return <output data-testid="current-route">{`${location.pathname}${location.search}`}</output>;
}

function renderIssuePage(path = "/issues/CORE-1", navigateTo?: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <CurrentRoute />
        {navigateTo === undefined ? null : <Link to={navigateTo}>Navigate to test route</Link>}
        <Routes>
          <Route
            path="/issues/:key/*"
            element={<IssuePage user={{ kind: "user", login: "alice" }} />}
          />
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
            <Route
              path="/issues/:key/*"
              element={<IssuePage user={{ kind: "user", login: "alice" }} />}
            />
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

test("IssuePage keeps the document provider alive while switching tabs", async () => {
  const restore = stubIssuePage(issue);
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const destroyProvider = spyOn(HocuspocusProvider.prototype, "destroy");
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    await screen.findByRole("tab", { name: "Spec", selected: true });
    await waitFor(() => expect(document.querySelector(".cm-editor")).not.toBeNull());

    fireEvent.click(screen.getByRole("tab", { name: "Log" }));
    await screen.findByRole("tab", { name: "Log", selected: true });
    fireEvent.click(screen.getByRole("tab", { name: "Spec" }));
    await screen.findByRole("tab", { name: "Spec", selected: true });

    expect(destroyProvider).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    destroyProvider.mockRestore();
    globalThis.WebSocket = originalWebSocket;
    restore();
  }
});

test("IssuePage preserves an in-progress Log composer draft across a tab round-trip", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1");

  try {
    await screen.findByRole("tab", { name: "Log", selected: true });
    const composer = (await screen.findByLabelText("Comment")) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: "Draft in progress" } });
    expect(composer.value).toBe("Draft in progress");

    fireEvent.click(screen.getByRole("tab", { name: "Spec" }));
    await screen.findByRole("tab", { name: "Spec", selected: true });
    fireEvent.click(screen.getByRole("tab", { name: "Log" }));
    await screen.findByRole("tab", { name: "Log", selected: true });

    expect((screen.getByLabelText("Comment") as HTMLTextAreaElement).value).toBe(
      "Draft in progress"
    );
  } finally {
    view.unmount();
    restore();
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
            <Route
              path="/issues/:key/*"
              element={<IssuePage user={{ kind: "user", login: "alice" }} />}
            />
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

class IssuePageWebSocketStub {
  binaryType = "arraybuffer";
  identifier = 0;
  readyState = 0;

  addEventListener(..._args: unknown[]): void {}

  close(): void {
    this.readyState = 3;
  }

  removeEventListener(..._args: unknown[]): void {}

  send(..._args: unknown[]): void {}
}

test("IssuePage highlights a current spec range from its deep-link URL", async () => {
  const primaryArtifact = issue.artifacts?.[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = IssuePageWebSocketStub as unknown as typeof WebSocket;
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "SQLite is local",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/spec?from=0&to=5");

  try {
    await waitFor(() =>
      expect(view.container.querySelector("mark.dispatch-anchor-history")?.textContent).toBe(
        "SQLit"
      )
    );
    expect(screen.getByRole("button", { name: "Edit" })).not.toBeNull();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
    globalThis.WebSocket = originalWebSocket;
  }
});

test("IssuePage shows the orphan affordance for an unmapped current spec deep link", async () => {
  const primaryArtifact = issue.artifacts?.[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = IssuePageWebSocketStub as unknown as typeof WebSocket;
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "SQLite is local",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/spec?from=20&to=25");

  try {
    await within(view.container).findByText(
      "Text changed. The selected range no longer exists in this document."
    );
    expect(view.container.querySelector("mark.dispatch-anchor-history")).toBeNull();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
    globalThis.WebSocket = originalWebSocket;
  }
});

test("IssuePage renders an image artifact in the Artifacts tab", async () => {
  const imageArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-image",
    issue_key: "CORE-1",
    kind: "image" as const,
    name: "diagram.png",
    primary: false,
    slug: "diagram-png",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "image/png",
        named: false,
        number: 1,
        sha256: "image-sha",
        size: 10,
        summary: null,
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, imageArtifact] });
  const view = renderIssuePage("/issues/CORE-1/artifacts/diagram-png");

  try {
    await screen.findByRole("tab", { name: "Artifacts", selected: true });
    expect(screen.getByRole("img", { name: "diagram.png version 1" })).not.toBeNull();
    expect(screen.getByTestId("artifact-header").className).toContain("ring-2");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage does not mount an image as the hidden Spec document", async () => {
  const imageArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-image",
    issue_key: "CORE-1",
    kind: "image" as const,
    name: "diagram.png",
    primary: false,
    slug: "diagram-png",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "image/png",
        named: false,
        number: 1,
        sha256: "image-sha",
        size: 10,
        summary: null,
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, imageArtifact] });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/spec", "/issues/CORE-1/artifacts/diagram-png");

  try {
    await screen.findByRole("tab", { name: "Spec", selected: true });
    await waitFor(() => expect(getArtifactText).toHaveBeenCalledWith("artifact-1"));
    getArtifactText.mockClear();
    fireEvent.click(screen.getByRole("link", { name: "Navigate to test route" }));
    await screen.findByRole("img", { name: "diagram.png version 1" });
    expect(getArtifactText).not.toHaveBeenCalledWith("artifact-image");
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage opens a non-spec document at its version route", async () => {
  const documentArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-design",
    issue_key: "CORE-1",
    kind: "doc" as const,
    name: "design.md",
    primary: false,
    slug: "design",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "text/markdown",
        named: true,
        number: 1,
        sha256: "document-sha",
        size: 10,
        summary: "Initial design",
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, documentArtifact] });
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...documentArtifact,
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Design",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/artifacts/design");

  try {
    await screen.findByRole("tab", { name: "Artifacts", selected: true });
    expect(screen.getByRole("heading", { name: "design.md" })).not.toBeNull();
    expect(screen.queryByLabelText("Artifact version")).toBeNull();
    fireEvent.change(screen.getByLabelText("Version"), { target: { value: "1" } });
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        "/issues/CORE-1/artifacts/design?v=1"
      )
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage shows an unavailable image version instead of the latest image", async () => {
  const imageArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-image",
    issue_key: "CORE-1",
    kind: "image" as const,
    name: "diagram.png",
    primary: false,
    slug: "diagram-png",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "image/png",
        named: false,
        number: 1,
        sha256: "image-sha",
        size: 10,
        summary: null,
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, imageArtifact] });
  const view = renderIssuePage("/issues/CORE-1/artifacts/diagram-png?v=999");

  try {
    await screen.findByRole("tab", { name: "Artifacts", selected: true });
    expect(screen.getByText("Version 999 is not available for this artifact.")).not.toBeNull();
    expect(screen.queryByRole("img", { name: "diagram.png version 1" })).toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage explains when an artifact slug does not exist", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/artifacts/missing");

  try {
    await screen.findByText("No artifact missing on CORE-1.");
    expect(screen.getByRole("link", { name: "View artifacts" }).getAttribute("href")).toBe(
      "/issues/CORE-1/artifacts"
    );
    expect(screen.getByRole("tablist", { name: "Issue detail" })).not.toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage hides the Artifacts panel after switching to Spec", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/artifacts");

  try {
    await screen.findByRole("tab", { name: "Artifacts", selected: true });
    fireEvent.click(screen.getByRole("tab", { name: "Spec" }));
    await screen.findByRole("tab", { name: "Spec", selected: true });
    const artifactsPanel = view.container.querySelector<HTMLDivElement>("#issue-artifacts-panel");
    expect(artifactsPanel?.hidden).toBe(true);
    expect(artifactsPanel?.getAttribute("aria-hidden")).toBe("true");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage encodes primary-document version selection in the artifact route", async () => {
  const primaryArtifact = issue.artifacts[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const versionedIssue = {
    ...issue,
    artifacts: [
      {
        ...primaryArtifact,
        versions: [
          {
            authors: [{ id: "alice", kind: "user" as const }],
            created_at: "2026-09-09T01:00:00Z",
            mime: "text/markdown",
            named: true,
            number: 1,
            sha256: "primary-sha",
            size: 10,
            summary: "Initial spec",
          },
        ],
      },
    ],
  };
  const restore = stubIssuePage(versionedIssue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...versionedIssue.artifacts[0],
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    await screen.findByLabelText("Version");
    fireEvent.change(screen.getByLabelText("Version"), { target: { value: "1" } });
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        "/issues/CORE-1/artifacts/spec?v=1"
      )
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage shows a failed historical document version", async () => {
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockRejectedValue(
    new Error("missing version")
  );
  const view = renderIssuePage("/issues/CORE-1/artifacts/spec?v=999");

  try {
    await screen.findByText("No version 999 of spec.md.");
    expect(screen.getByRole("link", { name: "View current version" }).getAttribute("href")).toBe(
      "/issues/CORE-1/spec"
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactVersion.mockRestore();
    restore();
  }
});
