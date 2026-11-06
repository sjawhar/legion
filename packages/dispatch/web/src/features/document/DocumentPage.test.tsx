import { afterAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import type { Artifact, Subscriber } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { appKeymap } from "../shell/keymap";
import { DocumentPage } from "./DocumentPage";

const getReferences = spyOn(api, "getReferences").mockResolvedValue({
  edges: [],
  node: { id: "artifact-1", kind: "artifact" },
});

afterAll(() => {
  getReferences.mockRestore();
});

const artifact: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: null,
  kind: "doc",
  name: "Design notes",
  primary: false,
  project: "CORE",
  slug: "design-notes",
  versions: [
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-10T00:00:00Z",
      named: false,
      number: 1,
      summary: null,
    },
  ],
};

function renderDocumentPage(path = "/projects/CORE/documents/design-notes") {
  const runtime = fakeDocumentRuntime({ text: "# Design notes" });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={runtime.runtime}>
          <MarginProvider>
            <Routes>
              <Route element={<DocumentPage />} path="/projects/:key/documents/:slug" />
            </Routes>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { runtime, view };
}

test("renders the header, the document, versions, and Referenced by for an unlinked document", async () => {
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue(artifact);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockResolvedValue([]);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Design notes",
    version: 1,
    token: "sha256:design-notes",
  });
  getReferences.mockResolvedValueOnce({
    edges: [
      {
        created_at: "2026-09-10T00:00:00Z",
        direction: "in",
        excerpt: { text: "See dispatch://CORE/artifact/design-notes" },
        kind: "mentions",
        node: {
          id: "comment-1",
          issue_key: "CORE-1",
          kind: "comment",
          project: "CORE",
          ref: "dispatch://CORE-1/comment/comment-1",
        },
        source_seq: 1,
      },
    ],
    node: {
      id: "artifact-1",
      kind: "artifact",
      project: "CORE",
      ref: "dispatch://CORE/artifact/design-notes",
    },
  });
  const { runtime, view } = renderDocumentPage();

  try {
    await screen.findByTestId("artifact-header");
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    await screen.findByRole("textbox", { name: "Document editor" });

    expect(screen.getByRole("link", { name: "Project CORE" }).getAttribute("href")).toBe(
      "/projects/CORE/documents"
    );
    expect(screen.queryByRole("combobox", { name: "Artifact version" })).toBeNull();
    expect(screen.getByRole("combobox", { name: "Version" })).not.toBeNull();
    const referencedBy = screen.getByRole("region", { name: "Referenced by" });
    // Nothing discloses this panel, so it is its own heading and carries the count.
    expect(within(referencedBy).getByRole("heading", { name: "Referenced by (1)" })).toBeTruthy();
    expect(referencedBy.textContent).toContain("Comment · CORE-1");
    expect(
      within(referencedBy).getByRole("link", { name: "Comment · CORE-1" }).getAttribute("href")
    ).toBe("/issues/CORE-1/comments/comment-1");
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
    getArtifactSubscribers.mockRestore();
    getArtifact.mockRestore();
    getProjectArtifact.mockRestore();
  }
});

test("shows Document not found with a way back for an unknown slug", async () => {
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockRejectedValue(
    new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "not found" })
  );
  const { view } = renderDocumentPage();

  try {
    await screen.findByRole("heading", { name: "Document not found" });
    expect(screen.getByRole("link", { name: "Back to CORE documents" }).getAttribute("href")).toBe(
      "/projects/CORE/documents"
    );
  } finally {
    view.unmount();
    getProjectArtifact.mockRestore();
  }
});

test("shows Subscribed agents and unsubscribes after confirming the dialog", async () => {
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue(artifact);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Design notes",
    version: 1,
    token: "sha256:design-notes",
  });
  let currentSubscribers: Subscriber[] = [
    {
      last_seen: 1,
      live: true,
      removable: true,
      session_id: "0123456789abcdef",
      title: "Planner (live)",
      topics: [],
    },
  ];
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockImplementation(
    async () => currentSubscribers
  );
  const unsubscribeArtifactSession = spyOn(api, "unsubscribeArtifactSession").mockImplementation(
    async () => {
      currentSubscribers = [];
    }
  );
  // The subscriber's running state is the agent registry's, which lists it.
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/workspaces/legion",
      host: "example-host-devbox",
      last_activity: null,
      last_seen: 1,
      machine_id: "example-host-devbox",
      open_asks: 0,
      roles: [],
      session_id: "0123456789abcdef",
      title: "Planner (live)",
    },
  ]);
  const { runtime, view } = renderDocumentPage();

  try {
    await screen.findByTestId("artifact-header");
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();

    const subscribed = await screen.findByRole("region", { name: "Subscribed agents" });
    const planner = await within(subscribed).findByRole("link", { name: "Planner (live)" });
    expect(planner.getAttribute("href")).toBe("/agents/0123456789abcdef/live");
    await waitFor(() =>
      expect(
        subscribed
          .querySelector('[data-agent-name="0123456789abcdef"]')
          ?.getAttribute("data-agent-state")
      ).toBe("running")
    );

    fireEvent.click(within(subscribed).getByRole("button", { name: "Unsubscribe" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain(
      "Unsubscribe Planner (live) from CORE/design-notes? They will be told."
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));

    await waitFor(() =>
      expect(unsubscribeArtifactSession).toHaveBeenCalledWith(
        "artifact-1",
        "0123456789abcdef",
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).toBeNull()
    );
  } finally {
    view.unmount();
    unsubscribeArtifactSession.mockRestore();
    listAgents.mockRestore();
    getArtifactSubscribers.mockRestore();
    getArtifactText.mockRestore();
    getArtifact.mockRestore();
    getProjectArtifact.mockRestore();
  }
});

/** The palette's Copy Markdown row, which is never offered twice. */
function copyMarkdownRow() {
  const rows = appKeymap.actions().filter((action) => action.label === "Copy Markdown");
  expect(rows.length).toBeLessThanOrEqual(1);
  return rows[0];
}

test("a project document copies its Markdown beside its link, the current text or the picked version's", async () => {
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue(artifact);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockResolvedValue([]);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Design notes\n\nUse Postgres.\n",
    version: 1,
    token: "sha256:design-notes",
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-10T00:00:00Z",
    markdown: "# Design notes\n\nUse SQLite.\n",
    named: false,
    number: 1,
    summary: null,
  });

  try {
    const current = renderDocumentPage();
    try {
      const header = await screen.findByTestId("artifact-header");
      const copy = await within(header).findByRole("button", { name: "Copy Markdown" });
      expect(
        copy.closest("[data-copy-markdown]")?.previousElementSibling?.matches("[data-copy-ref]")
      ).toBe(true);
      // The copy reads the text again when pressed, so an edit since the page opened is in it.
      getArtifactText.mockResolvedValue({
        markdown: "# Design notes\n\nUse Postgres 18.\n",
        version: 1,
        token: "sha256:design-notes-edited",
      });
      fireEvent.click(copy);
      await waitFor(() =>
        expect(writeText).toHaveBeenLastCalledWith("# Design notes\n\nUse Postgres 18.\n")
      );
      expect(await within(header).findByText("Copied", { exact: true })).toBeTruthy();
      expect(copyMarkdownRow()?.scope).toBe("document");
    } finally {
      current.view.unmount();
    }

    const past = renderDocumentPage("/projects/CORE/documents/design-notes?version=1");
    try {
      const header = await screen.findByTestId("artifact-header");
      const copy = await within(header).findByRole("button", { name: "Copy Markdown" });
      fireEvent.click(copy);
      await waitFor(() =>
        expect(writeText).toHaveBeenLastCalledWith("# Design notes\n\nUse SQLite.\n")
      );
      expect(getArtifactVersion).toHaveBeenCalledWith("artifact-1", 1);
    } finally {
      past.view.unmount();
    }
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    getArtifactVersion.mockRestore();
    getArtifactText.mockRestore();
    getArtifactSubscribers.mockRestore();
    getArtifact.mockRestore();
    getProjectArtifact.mockRestore();
  }
});

test("a project's uploaded file that is not Markdown has its link button and no Copy Markdown", async () => {
  const file: Artifact = {
    ...artifact,
    kind: "file",
    name: "trace.json",
    slug: "trace-json",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" }],
        created_at: "2026-09-10T00:00:00Z",
        mime: "application/json",
        named: false,
        number: 1,
        sha256: "trace-sha",
        size: 12,
        summary: null,
      },
    ],
  };
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue(file);
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockResolvedValue([]);
  const { view } = renderDocumentPage("/projects/CORE/documents/trace-json");

  try {
    const header = await screen.findByTestId("artifact-header");
    await screen.findByRole("link", { name: "Download version 1" });
    expect(within(header).getByRole("button", { name: /^Copy reference / })).toBeTruthy();
    expect(within(header).queryByRole("button", { name: "Copy Markdown" })).toBeNull();
    expect(copyMarkdownRow()).toBeUndefined();
  } finally {
    view.unmount();
    getArtifactSubscribers.mockRestore();
    getProjectArtifact.mockRestore();
  }
});
