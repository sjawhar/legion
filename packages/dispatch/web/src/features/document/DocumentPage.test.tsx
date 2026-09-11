import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { ApiError, api } from "../../api/client";
import type { ArtifactDetails, Subscriber } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/Margin";
import { DocumentPage } from "./DocumentPage";

const artifact: ArtifactDetails = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: null,
  kind: "doc",
  name: "Design notes",
  primary: false,
  project: "CORE",
  referenced_by: [
    {
      excerpt: "See dispatch://CORE/artifact/design-notes",
      id: "comment-1",
      issue_key: "CORE-1",
      kind: "comment",
      project: "CORE",
    },
  ],
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

function renderDocumentPage() {
  const runtime = fakeDocumentRuntime({ text: "# Design notes" });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={["/projects/CORE/documents/design-notes"]}>
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
    expect(referencedBy.textContent).toContain("Comment · CORE-1");
    expect(
      within(referencedBy).getByRole("link", { name: "Comment · CORE-1" }).getAttribute("href")
    ).toBe("/issues/CORE-1");
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
  const { runtime, view } = renderDocumentPage();

  try {
    await screen.findByTestId("artifact-header");
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();

    const subscribed = await screen.findByRole("region", { name: "Subscribed agents" });
    await within(subscribed).findByText("Planner (live)");
    expect(within(subscribed).getAllByTitle("Live")).toHaveLength(1);

    fireEvent.click(within(subscribed).getByRole("button", { name: "Unsubscribe" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain(
      "Unsubscribe Planner (live) from CORE/design-notes? They will be told."
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));

    await waitFor(() =>
      expect(unsubscribeArtifactSession).toHaveBeenCalledWith("artifact-1", "0123456789abcdef")
    );
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).toBeNull()
    );
  } finally {
    view.unmount();
    unsubscribeArtifactSession.mockRestore();
    getArtifactSubscribers.mockRestore();
    getArtifactText.mockRestore();
    getArtifact.mockRestore();
    getProjectArtifact.mockRestore();
  }
});
