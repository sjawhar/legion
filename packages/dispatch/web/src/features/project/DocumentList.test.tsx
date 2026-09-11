import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { DocumentList } from "./DocumentList";

function document(overrides: Partial<Artifact> = {}): Artifact {
  return {
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
        created_at: "2026-09-10T00:05:00Z",
        named: false,
        number: 1,
        summary: null,
      },
    ],
    ...overrides,
  };
}

function Location() {
  return <output data-testid="location">{useLocation().pathname}</output>;
}

function uploadResponse() {
  const artifact = document();
  const version = artifact.versions.at(0);
  if (version === undefined) {
    throw new Error("document fixture requires a version");
  }
  return { artifact, version };
}

function renderList(artifacts: Artifact[]) {
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue(artifacts);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentList project="CORE" />
        <Location />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { listProjectArtifacts, view };
}

test("Documents requests unlinked project artifacts and links a document to its project page", async () => {
  const { listProjectArtifacts, view } = renderList([document()]);

  try {
    const unlinked = await screen.findByRole("listitem", { name: /Design notes/ });
    expect(unlinked.textContent).toContain("doc");
    expect(unlinked.querySelector("time")).not.toBeNull();
    expect(screen.getByRole("link", { name: "Design notes" }).getAttribute("href")).toBe(
      "/projects/CORE/documents/design-notes"
    );
    expect(listProjectArtifacts).toHaveBeenCalledWith("CORE", true);
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
  }
});

test("New document opens its document page", async () => {
  const { listProjectArtifacts, view } = renderList([]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(uploadResponse());

  try {
    await screen.findByText("No documents yet.");
    fireEvent.click(screen.getByRole("button", { name: "New document" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
      target: { value: "Design notes" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() =>
      expect(uploadArtifact).toHaveBeenCalledWith(
        { project: "CORE" },
        { content: "# Design notes\n", name: "Design notes" }
      )
    );
    expect(screen.getByTestId("location").textContent).toBe(
      "/projects/CORE/documents/design-notes"
    );
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});

test("New document refuses a name that already exists", async () => {
  const { listProjectArtifacts, view } = renderList([document()]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(uploadResponse());

  try {
    await screen.findByText("Design notes");
    fireEvent.click(screen.getByRole("button", { name: "New document" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
      target: { value: "Design notes" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    const error = await screen.findByRole("alert");
    expect(error.textContent).toContain("A document named Design notes already exists");
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listProjectArtifacts.mockRestore();
    uploadArtifact.mockRestore();
  }
});
