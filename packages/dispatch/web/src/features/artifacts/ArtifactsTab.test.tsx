import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { ArtifactsTab, ReferencedBy } from "./ArtifactsTab";

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

test("ArtifactsTab compares selected blob versions side by side", async () => {
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([artifact]);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...artifact,
    referenced_by: [],
  });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });

  try {
    render(
      <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
        <QueryClientProvider client={queryClient}>
          <ArtifactsTab />
        </QueryClientProvider>
      </MemoryRouter>
    );

    await screen.findByTestId("artifact-diagram-png");
    fireEvent.change(screen.getByLabelText("Compare diagram.png from"), {
      target: { value: "1" },
    });
    fireEvent.change(screen.getByLabelText("Compare diagram.png to"), {
      target: { value: "2" },
    });

    const comparison = await screen.findByLabelText("Blob version comparison");
    expect(comparison.textContent).toContain("Version 1");
    expect(comparison.textContent).toContain("10 B");
    expect(comparison.textContent).toContain("first-sha");
    expect(comparison.textContent).toContain("Version 2");
    expect(comparison.textContent).toContain("20 B");
    expect(comparison.textContent).toContain("second-sha");
    fireEvent.change(screen.getByLabelText("Compare diagram.png to"), {
      target: { value: "1" },
    });
    expect(await screen.findByText("Identical blobs.")).not.toBeNull();
  } finally {
    listArtifacts.mockRestore();
    getArtifact.mockRestore();
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
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue(artifacts);
  const getArtifact = spyOn(api, "getArtifact").mockImplementation(async (id) => {
    const artifact = artifacts.find((candidate) => candidate.id === id);
    if (!artifact) throw new Error(`missing artifact ${id}`);
    return { ...artifact, referenced_by: [] };
  });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });

  try {
    render(
      <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
        <QueryClientProvider client={queryClient}>
          <ArtifactsTab />
        </QueryClientProvider>
      </MemoryRouter>
    );

    await screen.findByTestId("artifact-spec");
    expect(screen.getByTestId("artifact-spec").textContent).toContain("Primary");
    expect(screen.queryByRole("button", { name: /make primary/i })).toBeNull();
    expect(screen.queryByText(/Not\s+primary/)).toBeNull();
  } finally {
    listArtifacts.mockRestore();
    getArtifact.mockRestore();
  }
});
test("References lists closure members with chip, via, and depth, names truncation, and renders an artifact source without an issue key", async () => {
  const referencedDocument: Artifact = {
    ...artifact,
    id: "artifact-notes",
    issue_key: null,
    kind: "doc",
    name: "Design notes",
    primary: false,
    slug: "design-notes",
  };
  const listArtifacts = spyOn(api, "listArtifacts").mockResolvedValue([artifact]);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...artifact,
    referenced_by: [
      {
        excerpt: "Design notes links here",
        id: "artifact-notes",
        issue_key: null,
        kind: "artifact",
        project: "CORE",
        ref_key: "CORE/design-notes",
      },
    ],
  });
  const getIssueReferences = spyOn(api, "getIssueReferences").mockResolvedValue({
    members: [
      {
        artifact: referencedDocument,
        depth: 1,
        via: { id: "comment-1", kind: "comment" },
      },
    ],
    truncated: true,
  });
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
  try {
    const references = await within(view.container).findByRole("region", { name: "References" });
    expect(
      within(references).getByRole("link", { name: "Design notes" }).getAttribute("href")
    ).toBe("/projects/CORE/documents/design-notes");
    expect(references.textContent).toContain("CORE");
    expect(references.textContent).toContain("via comment");
    expect(references.textContent).toContain("depth 1");
    expect(references.textContent).toContain("more references beyond 8 hops");

    const referencedBy = within(view.container).getByRole("region", { name: "Referenced by" });
    expect(
      within(referencedBy)
        .getByRole("link", { name: "Artifact · CORE/design-notes" })
        .getAttribute("href")
    ).toBe("/projects/CORE/documents/design-notes");
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getIssueReferences.mockRestore();
    listArtifacts.mockRestore();
  }
});

test("Referenced by renders an artifact source with an issue key through its artifact path", () => {
  const view = render(
    <MemoryRouter>
      <ReferencedBy
        references={[
          {
            excerpt: "Source artifact",
            id: "artifact-source",
            issue_key: "CORE-1",
            kind: "artifact",
            project: "CORE",
            ref_key: "CORE-1/source-doc",
          },
        ]}
      />
    </MemoryRouter>
  );

  try {
    expect(screen.getByRole("link", { name: "Artifact · CORE-1" }).getAttribute("href")).toBe(
      "/issues/CORE-1/artifacts/source-doc"
    );
  } finally {
    view.unmount();
  }
});
