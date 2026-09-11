import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, IssueReferences } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { ArtifactsTab } from "./ArtifactsTab";

const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "image",
  name: "diagram.png",
  primary: false,
  slug: "diagram-png",
  versions: [
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-09T00:00:00Z",
      mime: "image/png",
      named: false,
      number: 1,
      sha256: "first-sha",
      size: 10,
      summary: null,
    },
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-09T01:00:00Z",
      mime: "image/png",
      named: false,
      number: 2,
      sha256: "second-sha",
      size: 20,
      summary: null,
    },
  ],
};

const noReferences: IssueReferences = { members: [], truncated: false };

function renderTab(artifacts: Artifact[], references: IssueReferences = noReferences) {
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue(artifacts);
  const getIssueReferences = spyOn(api, "getIssueReferences").mockResolvedValue(references);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "artifacts" })]}>
      <QueryClientProvider client={queryClient}>
        <ArtifactsTab />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getIssueReferences, listArtifacts, view };
}

test("renders one row per artifact with no compare controls", async () => {
  const notes: Artifact = {
    ...artifact,
    id: "artifact-notes",
    kind: "doc",
    name: "notes.md",
    slug: "notes-md",
    versions: artifact.versions.slice(0, 1),
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact, notes]);

  try {
    await screen.findByTestId("artifact-diagram-png");
    expect(screen.getByTestId("artifact-notes-md")).not.toBeNull();
    expect(screen.queryByLabelText(/Compare .* from/)).toBeNull();
    expect(screen.queryByLabelText(/Compare .* to/)).toBeNull();
    expect(screen.queryByText("Compare versions")).toBeNull();
    expect(screen.queryByRole("button", { name: "Show all versions" })).toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("artifacts tab renders no make-primary control", async () => {
  const artifacts: Artifact[] = [
    {
      ...artifact,
      id: "artifact-spec",
      kind: "doc",
      name: "spec.md",
      primary: true,
      slug: "spec",
      versions: artifact.versions.slice(0, 1),
    },
    {
      ...artifact,
      id: "artifact-notes",
      kind: "doc",
      name: "notes.md",
      primary: false,
      slug: "notes-md",
      versions: artifact.versions.slice(0, 1),
    },
    artifact,
  ];
  const { getIssueReferences, listArtifacts, view } = renderTab(artifacts);

  try {
    await screen.findByTestId("artifact-spec");
    expect(screen.getByTestId("artifact-spec").textContent).toContain("Primary");
    expect(screen.queryByRole("button", { name: /make primary/i })).toBeNull();
    expect(screen.queryByText(/Not\s+primary/)).toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("a primary artifact sorts first regardless of upload order", async () => {
  const older: Artifact = {
    ...artifact,
    id: "artifact-older",
    name: "older.png",
    slug: "older-png",
  };
  const primary: Artifact = {
    ...artifact,
    id: "artifact-primary",
    name: "primary.png",
    primary: true,
    slug: "primary-png",
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([older, primary]);

  try {
    await screen.findByTestId("artifact-older-png");
    const rows = screen.getAllByRole("listitem");
    expect(rows[0]?.getAttribute("data-testid")).toBe("artifact-primary-png");
    expect(rows[1]?.getAttribute("data-testid")).toBe("artifact-older-png");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("the filter input narrows rows by name and kind", async () => {
  const notes: Artifact = {
    ...artifact,
    id: "artifact-notes",
    kind: "doc",
    name: "notes.md",
    slug: "notes-md",
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact, notes]);

  try {
    await screen.findByTestId("artifact-diagram-png");
    fireEvent.change(screen.getByLabelText("Filter artifacts"), { target: { value: "notes" } });
    expect(screen.queryByTestId("artifact-diagram-png")).toBeNull();
    expect(screen.getByTestId("artifact-notes-md")).not.toBeNull();

    fireEvent.change(screen.getByLabelText("Filter artifacts"), { target: { value: "image" } });
    expect(screen.getByTestId("artifact-diagram-png")).not.toBeNull();
    expect(screen.queryByTestId("artifact-notes-md")).toBeNull();
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});

test("References lists closure members with chip, via, and depth, names truncation, and renders an artifact source without an issue key, collapsed by default", async () => {
  const referencedDocument: Artifact = {
    ...artifact,
    id: "artifact-notes",
    issue_key: null,
    kind: "doc",
    name: "Design notes",
    primary: false,
    slug: "design-notes",
  };
  const { getIssueReferences, listArtifacts, view } = renderTab([artifact], {
    members: [
      {
        artifact: referencedDocument,
        depth: 1,
        via: { id: "comment-1", kind: "comment" },
      },
    ],
    truncated: true,
  });

  try {
    const summary = await screen.findByText("References (1)");
    const details = summary.closest("details");
    expect(details).toBeInstanceOf(HTMLDetailsElement);
    expect((details as HTMLDetailsElement).open).toBe(false);

    const references = within(view.container).getByRole("region", { name: "References" });
    expect(
      within(references).getByRole("link", { name: "Design notes" }).getAttribute("href")
    ).toBe("/projects/CORE/documents/design-notes");
    expect(references.textContent).toContain("CORE");
    expect(references.textContent).toContain("via comment");
    expect(references.textContent).toContain("depth 1");
    expect(references.textContent).toContain("more references beyond 8 hops");
  } finally {
    view.unmount();
    listArtifacts.mockRestore();
    getIssueReferences.mockRestore();
  }
});
