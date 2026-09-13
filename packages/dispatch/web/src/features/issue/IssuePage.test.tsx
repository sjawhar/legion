import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Link, MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { api } from "../../api/client";
import type { Ask, InboxRow, IssueDetails, Subscriber } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/Margin";
import { IssuePage } from "./IssuePage";

const issue: IssueDetails = {
  artifacts: [
    {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      project: "CORE",
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
  priority: null,
  rank: "U",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};

function subscriber(sessionId: string, title: string, live = true): Subscriber {
  return {
    last_seen: 1,
    live,
    removable: true,
    session_id: sessionId,
    title,
    topics: [],
  };
}

const openIssueAsk: Ask = {
  anchor: null,
  answer: null,
  author: { id: "session-1", kind: "session" },
  created_at: "2026-09-10T00:00:00Z",
  edited_at: null,
  id: "ask-open",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Should this ship?",
  state: "open",
  urgency: "med",
};

function issueWithExternalLink(url: string): IssueDetails {
  return { ...issue, external_links: [{ url }] };
}

function stubIssuePage(
  nextIssue: IssueDetails,
  inbox: InboxRow[] = [],
  subscribers: Subscriber[] = []
): () => void {
  const originalGetIssue = api.getIssue;
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetInbox = api.getInbox;
  const originalGetMyState = api.getMyState;
  const originalListAgents = api.listAgents;
  const originalGetIssueSubscribers = api.getIssueSubscribers;
  api.getIssue = async () => nextIssue;
  api.getInbox = async () => inbox;
  api.getMyState = async () => ({ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false } });
  api.getIssueEvents = async () => [];
  api.listAgents = async () => [];
  api.getIssueSubscribers = async () => subscribers;
  return () => {
    api.getIssue = originalGetIssue;
    api.getIssueEvents = originalGetIssueEvents;
    api.getInbox = originalGetInbox;
    api.getMyState = originalGetMyState;
    api.listAgents = originalListAgents;
    api.getIssueSubscribers = originalGetIssueSubscribers;
  };
}

function CurrentRoute() {
  const location = useLocation();
  return <output data-testid="current-route">{`${location.pathname}${location.search}`}</output>;
}

function renderIssuePage(
  path = "/issues/CORE-1",
  navigateTo?: string,
  seedText?: string,
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  })
) {
  const runtime = fakeDocumentRuntime({ text: seedText });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={runtime.runtime}>
          <MarginProvider>
            <CurrentRoute />
            {navigateTo === undefined ? null : <Link to={navigateTo}>Navigate to test route</Link>}
            <Routes>
              <Route path="/issues/:key/*" element={<IssuePage />} />
            </Routes>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  window.setTimeout(runtime.sync);
  return { ...view, runtime };
}

async function openSubscribedAgents(count: number) {
  const toggle = await screen.findByRole("button", { name: `Subscribers: ${count}` });
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  fireEvent.click(toggle);
  expect(toggle.getAttribute("aria-expanded")).toBe("true");
  return screen.findByRole("region", { name: "Subscribed agents" });
}

test("IssuePage shows subscribed agents with a live indicator and their title", async () => {
  const restore = stubIssuePage(
    issue,
    [],
    [
      subscriber("0123456789abcdef", "Planner (live)", true),
      subscriber("fedcba9876543210", "Historical (offline)", false),
    ]
  );
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(2);
    expect(within(subscribed).getAllByRole("listitem")).toHaveLength(2);
    expect(within(subscribed).getByText("Planner (live)").getAttribute("title")).toBe(
      "0123456789abcdef"
    );
    expect(within(subscribed).getByText("Historical (offline)")).toBeDefined();
    expect(within(subscribed).getAllByTitle("Live")).toHaveLength(1);
    expect(within(subscribed).getAllByTitle("Not live")).toHaveLength(1);
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage disables Unsubscribe and explains why for a subscriber reachable only via a broader wildcard", async () => {
  const restore = stubIssuePage(
    issue,
    [],
    [
      {
        last_seen: 1,
        live: true,
        removable: false,
        session_id: "0123456789abcdef",
        title: "Wide",
        topics: ["notifications.dispatch.>"],
        via: "notifications.dispatch.>",
      },
    ]
  );
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    expect(within(subscribed).getByText("via notifications.dispatch.>")).toBeDefined();
    const unsubscribeButton = within(subscribed).getByRole("button", { name: "Unsubscribe" });
    expect(unsubscribeButton).toHaveProperty("disabled", true);
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage shortens a subscriber id when its title is blank", async () => {
  const restore = stubIssuePage(issue, [], [subscriber("0123456789abcdef", " ")]);
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    const label = within(subscribed).getByText("session:01234567…");
    expect(label.getAttribute("title")).toBe("0123456789abcdef");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage hides the Subscribed agents header when there are no subscribers", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage();

  try {
    await screen.findByText(issue.title);
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).toBeNull()
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage edits label chips with project suggestions and cancels unsaved changes", async () => {
  const labeledIssue = { ...issue, labels: ["bug"] };
  const restore = stubIssuePage(labeledIssue);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    { ...labeledIssue, open_asks: 0 },
    { ...issue, key: "CORE-2", labels: ["frontend", "backend"], open_asks: 0 },
  ]);
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue({
    ...labeledIssue,
    labels: ["frontend", "urgent"],
  });
  const view = renderIssuePage();

  try {
    fireEvent.click(await screen.findByRole("button", { name: "bug" }));
    const input = await screen.findByRole("textbox", { name: "Add label" });
    await waitFor(() => expect(listIssues).toHaveBeenCalledWith({ project: "CORE" }));
    fireEvent.click(await screen.findByRole("button", { name: "Add frontend" }));
    fireEvent.change(input, { target: { value: "urgent" } });
    fireEvent.keyDown(input, { key: "Enter" });
    fireEvent.click(screen.getByRole("button", { name: "Remove bug" }));
    fireEvent.click(screen.getByRole("button", { name: "Save labels" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { labels: ["frontend", "urgent"] })
    );
    fireEvent.click(await screen.findByRole("button", { name: "frontend" }));
    const secondInput = await screen.findByRole("textbox", { name: "Add label" });
    fireEvent.change(secondInput, { target: { value: "backend" } });
    fireEvent.keyDown(secondInput, { key: "Enter" });
    fireEvent.keyDown(secondInput, { key: "Escape" });

    expect(screen.queryByRole("textbox", { name: "Add label" })).toBeNull();
    expect(patchIssue).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "backend" })).toBeNull();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    listIssues.mockRestore();
    restore();
  }
});

test("IssuePage shows a retryable error and hides the section when the subscribers lookup fails", async () => {
  const restore = stubIssuePage(issue);
  api.getIssueSubscribers = async () => {
    throw new Error("Envoy unavailable");
  };
  const view = renderIssuePage();

  try {
    await screen.findByText(issue.title);
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).toBeNull()
    );
    await screen.findByText("Subscribed agents unavailable — Envoy listener unreachable.");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage unsubscribes an agent after confirming the dialog", async () => {
  const restore = stubIssuePage(issue);
  let currentSubscribers = [subscriber("0123456789abcdef", "Planner (live)")];
  api.getIssueSubscribers = async () => currentSubscribers;
  const unsubscribeCalls: [string, string][] = [];
  api.unsubscribeIssueSession = async (key, sessionId) => {
    unsubscribeCalls.push([key, sessionId]);
    currentSubscribers = [];
  };
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    await within(subscribed).findByText("Planner (live)");
    fireEvent.click(within(subscribed).getByRole("button", { name: "Unsubscribe" }));

    const dialog = await screen.findByRole("dialog", { name: "Unsubscribe" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(unsubscribeCalls).toEqual([["CORE-1", "0123456789abcdef"]]));
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).toBeNull()
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage cancels an unsubscribe confirmation without calling the mutation", async () => {
  const restore = stubIssuePage(issue, [], [subscriber("0123456789abcdef", "Planner (live)")]);
  let unsubscribeCalled = false;
  api.unsubscribeIssueSession = async () => {
    unsubscribeCalled = true;
  };
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    fireEvent.click(within(subscribed).getByRole("button", { name: "Unsubscribe" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(unsubscribeCalled).toBe(false);
    expect(within(subscribed).getByText("Planner (live)")).toBeDefined();
  } finally {
    view.unmount();
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

test("IssuePage opens the Spec tab and leaves open asks out of the main column", async () => {
  const restore = stubIssuePage(issue, [{ ...openIssueAsk, priority: issue.priority }]);
  const view = renderIssuePage("/issues/CORE-1");

  try {
    await screen.findByRole("tab", { name: "Spec", selected: true });
    expect(screen.getByRole("tab", { name: "Conversation", selected: false })).toBeDefined();
    expect(screen.getByRole("tab", { name: "Children", selected: false })).toBeDefined();
    expect(screen.queryByLabelText("Issue board")).toBeNull();
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

test("IssuePage keeps the Spec mounted across tabs", async () => {
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "The mounted specification",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/spec", undefined, "The mounted specification");

  try {
    await screen.findByRole("tab", { name: "Spec", selected: true });
    await waitFor(() =>
      expect(within(view.container).getByRole("article").textContent).toContain(
        "The mounted specification"
      )
    );

    fireEvent.click(screen.getByRole("tab", { name: "Conversation" }));
    await screen.findByRole("tab", { name: "Conversation", selected: true });
    fireEvent.click(screen.getByRole("tab", { name: "Spec" }));
    await screen.findByRole("tab", { name: "Spec", selected: true });

    expect(within(view.container).getByRole("article").textContent).toContain(
      "The mounted specification"
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage preserves an in-progress Conversation composer draft across a tab round-trip", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("tab", { name: "Conversation", selected: true });
    const composer = (await screen.findByLabelText("Message")) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: "Draft in progress" } });
    expect(composer.value).toBe("Draft in progress");

    fireEvent.click(screen.getByRole("tab", { name: "Spec" }));
    await screen.findByRole("tab", { name: "Spec", selected: true });
    fireEvent.click(screen.getByRole("tab", { name: "Conversation" }));
    await screen.findByRole("tab", { name: "Conversation", selected: true });

    expect((screen.getByLabelText("Message") as HTMLTextAreaElement).value).toBe(
      "Draft in progress"
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage replaces the retired /log path with /conversation", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/log?x=1");

  try {
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        "/issues/CORE-1/conversation?x=1"
      )
    );
    await screen.findByRole("tab", { name: "Conversation", selected: true });
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
        project: "CORE",
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
  const originalGetIssueSubscribers = api.getIssueSubscribers;
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
    api.getIssueSubscribers = async () => [];

    const view = render(
      <MemoryRouter initialEntries={["/issues/CORE-1"]}>
        <QueryClientProvider client={queryClient}>
          <MarginProvider>
            <Link to="/issues/CORE-2">Go to CORE-2</Link>
            <Routes>
              <Route path="/issues/:key/*" element={<IssuePage />} />
            </Routes>
          </MarginProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );

    try {
      fireEvent.click(await screen.findByRole("button", { name: "Messages default to no route" }));
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
      expect(
        await screen.findByRole("button", { name: "Messages default to role:second-issue-route" })
      ).toBeDefined();
    } finally {
      view.unmount();
    }
  } finally {
    api.getIssue = originalGetIssue;
    api.getIssueEvents = originalGetIssueEvents;
    api.getInbox = originalGetInbox;
    api.getMyState = originalGetMyState;
    api.getIssueSubscribers = originalGetIssueSubscribers;
  }
});

test("IssuePage highlights a historical quote from its comment deep link", async () => {
  const primaryArtifact = issue.artifacts?.[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "SQLite is local",
    version: 1,
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "SQLite is local",
    named: false,
    number: 1,
    summary: null,
  });
  const listComments = spyOn(api, "listComments").mockResolvedValue([
    {
      anchor: {
        artifact_id: primaryArtifact.id,
        block_id: null,
        mark_id: "m-1",
        orphaned: false,
        quote: "SQLit",
        version: 1,
      },
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Check the storage engine.",
      created_at: "2026-09-09T00:00:00Z",
      id: "comment-1",
      issue_key: issue.key,
      reply_to: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
    },
  ]);
  const view = renderIssuePage("/issues/CORE-1/artifact/spec?v=1&comment=comment-1");

  try {
    await waitFor(() =>
      expect(
        view.runtime.editors.some((editor) => editor.markdown?.includes('data-id="comment-1"'))
      ).toBe(true)
    );
    expect(view.runtime.editors.some((editor) => editor.focused.includes("comment-1"))).toBe(true);
    expect(
      within(within(view.container).getByRole("tabpanel", { name: "Spec" })).getAllByRole("article")
    ).toHaveLength(1);
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactVersion.mockRestore();
    listComments.mockRestore();
    restore();
  }
});

test("IssuePage reports an ambiguous historical quote as changed text", async () => {
  const primaryArtifact = issue.artifacts?.[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "SQLite and SQLite",
    version: 1,
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "SQLite and SQLite",
    named: false,
    number: 1,
    summary: null,
  });
  const listComments = spyOn(api, "listComments").mockResolvedValue([
    {
      anchor: {
        artifact_id: primaryArtifact.id,
        mark_id: "m-1",
        block_id: null,
        orphaned: false,
        quote: "SQLit",
        version: 1,
      },
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Check the storage engine.",
      created_at: "2026-09-09T00:00:00Z",
      id: "comment-1",
      issue_key: issue.key,
      reply_to: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
    },
  ]);
  const view = renderIssuePage("/issues/CORE-1/artifact/spec?v=1&comment=comment-1");

  try {
    await within(view.container).findByText(
      "Text changed. The selected range no longer exists in this document."
    );
    expect(view.container.querySelector("mark.dispatch-anchor-history")).toBeNull();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactVersion.mockRestore();
    listComments.mockRestore();
    restore();
  }
});

test("IssuePage renders an image artifact in the Artifacts tab", async () => {
  const imageArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-image",
    issue_key: "CORE-1",
    project: "CORE",
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
    project: "CORE",
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
    project: "CORE",
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
    expect(screen.getAllByRole("combobox", { name: "Version" })).toHaveLength(1);
    fireEvent.change(screen.getByRole("combobox", { name: "Version" }), {
      target: { value: "1" },
    });
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
    project: "CORE",
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

test("IssuePage renders exactly one Version combobox on the Spec tab", async () => {
  const primaryArtifact = issue.artifacts[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    await screen.findByRole("combobox", { name: "Version" });
    expect(screen.getAllByRole("combobox", { name: "Version" })).toHaveLength(1);
    expect(screen.queryByTestId("artifact-header")).toBeNull();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});
test("IssuePage keeps version controls inside the Spec toolbar", async () => {
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
  });
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    const toolbar = await screen.findByTestId("spec-document-toolbar");
    expect(within(toolbar).getByRole("combobox", { name: "Version" })).not.toBeNull();
    expect(within(toolbar).getByRole("button", { name: "Name version" })).not.toBeNull();

    for (const tab of ["Conversation", "Children", "Artifacts"] as const) {
      fireEvent.click(screen.getByRole("tab", { name: tab }));
      await screen.findByRole("tab", { name: tab, selected: true });
      expect(screen.queryByTestId("spec-document-toolbar")).toBeNull();
      expect(screen.queryByRole("combobox", { name: "Version" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Name version" })).toBeNull();
    }
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage keeps a label-heavy metadata rail on one scrolling line", async () => {
  const restore = stubIssuePage({
    ...issue,
    labels: Array.from({ length: 20 }, (_, index) => `long-label-${index}-for-overflow`),
  });
  const view = renderIssuePage();

  try {
    const rail = await screen.findByTestId("issue-metadata-rail");
    expect(rail.classList.contains("flex-nowrap")).toBe(true);
    expect(rail.classList.contains("overflow-x-auto")).toBe(true);
    expect(rail.classList.contains("[scrollbar-gutter:stable]")).toBe(true);
    expect(
      within(rail)
        .getByRole("button", { name: "Messages default to no route" })
        .classList.contains("max-w-[14ch]")
    ).toBe(true);
    expect(within(rail).getByTestId("issue-labels").classList.contains("shrink-0")).toBe(true);
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage updates whose turn when a human clarification is latest", async () => {
  const waitingIssue = { ...issue, open_asks: [{ ...openIssueAsk, last_reply: null }] };
  const restore = stubIssuePage(waitingIssue);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = renderIssuePage("/issues/CORE-1", undefined, undefined, queryClient);

  try {
    expect(await screen.findByText("Waiting on you (1)")).not.toBeNull();
    queryClient.setQueryData<IssueDetails>(["issue", "CORE-1"], {
      ...waitingIssue,
      open_asks: [
        {
          ...openIssueAsk,
          last_reply: {
            author: { id: "alice", kind: "user" },
            created_at: "2026-09-11T01:00:00Z",
          },
        },
      ],
    });
    expect(await screen.findByText("Waiting on agents (1)")).not.toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});
test("IssuePage keeps pin with identity and mixes state with actions", async () => {
  const restore = stubIssuePage({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "awaiting" },
      },
    ],
  });
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    const title = await screen.findByRole("heading", { level: 1, name: issue.title });
    expect(title.classList.contains("text-xs")).toBe(true);
    expect(title.classList.contains("sm:text-xl")).toBe(true);
    expect(screen.getByText(issue.key).classList.contains("self-start")).toBe(true);
    const pin = screen.getByRole("button", { name: "Pin issue" });
    expect(title.parentElement?.contains(pin)).toBe(true);

    const stateActions = screen.getByTestId("issue-state-actions");
    expect(stateActions.classList.contains("flex-wrap")).toBe(true);
    expect(within(stateActions).getByRole("combobox", { name: "Status" })).not.toBeNull();
    expect(within(stateActions).getByRole("combobox", { name: "Priority" })).not.toBeNull();
    expect(within(stateActions).getByRole("button", { name: "Approve" })).not.toBeNull();
    expect(within(stateActions).getByRole("button", { name: "Close issue" })).not.toBeNull();
    expect(within(stateActions).queryByRole("button", { name: "Pin issue" })).toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});
test("IssuePage keeps an unrequested draft approval passive", async () => {
  const restore = stubIssuePage({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "draft" },
      },
    ],
  });
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    expect(await screen.findByRole("button", { name: "Draft" })).not.toBeNull();
    expect(screen.queryByRole("button", { name: /^Approve$/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Request changes" })).toBeNull();
  } finally {
    view.unmount();
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

test("IssuePage closes an issue and reopens it into Backlog", async () => {
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
    referenced_by: [],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "The mounted specification",
    version: 1,
  });
  const closeSave = Promise.withResolvers<IssueDetails>();
  const reopenSave = Promise.withResolvers<IssueDetails>();
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(async (_key, input) => {
    if (input.status === "done") return closeSave.promise;
    if (input.status === "backlog") return reopenSave.promise;
    throw new Error(`Unexpected issue update: ${JSON.stringify(input)}`);
  });
  const view = renderIssuePage();

  try {
    const openStatus = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    expect([...openStatus.options].map((option) => option.value)).not.toContain("done");

    fireEvent.click(screen.getByRole("button", { name: "Close issue" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "done" }));
    // While the write is in flight the control is disabled: no second close can be queued.
    expect(
      (screen.getByRole("button", { name: "Close issue" }) as HTMLButtonElement).disabled
    ).toBe(true);
    await act(async () => {
      closeSave.resolve({ ...issue, closed_at: "2026-09-12T10:00:00Z", status: "done" });
      await closeSave.promise;
    });

    expect(await screen.findByText("This issue is closed.")).toBeTruthy();
    const closedStatus = screen.getByLabelText("Status") as HTMLSelectElement;
    expect(closedStatus.disabled).toBe(true);
    expect(closedStatus.value).toBe("done");

    fireEvent.click(screen.getByRole("button", { name: "Reopen issue" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" })
    );
    expect(
      (screen.getByRole("button", { name: "Reopen issue" }) as HTMLButtonElement).disabled
    ).toBe(true);
    await act(async () => {
      reopenSave.resolve({ ...issue, status: "backlog" });
      await reopenSave.promise;
    });

    await waitFor(() => expect(screen.queryByText("This issue is closed.")).toBeNull());
    expect(((await screen.findByLabelText("Status")) as HTMLSelectElement).value).toBe("backlog");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage lets a human move a Todo issue to In progress", async () => {
  const restore = stubIssuePage(issue);
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue({
    ...issue,
    status: "in_progress",
  });
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    expect(status.value).toBe("todo");
    expect(
      [...status.options].map((option) => ({ text: option.text, value: option.value }))
    ).toEqual([
      { text: "Triage", value: "triage" },
      { text: "Icebox", value: "icebox" },
      { text: "Backlog", value: "backlog" },
      { text: "Todo", value: "todo" },
      { text: "In progress", value: "in_progress" },
      { text: "Testing", value: "testing" },
      { text: "Needs review", value: "needs_review" },
      { text: "Retro", value: "retro" },
    ]);

    fireEvent.change(status, { target: { value: "in_progress" } });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "in_progress" })
    );
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});
