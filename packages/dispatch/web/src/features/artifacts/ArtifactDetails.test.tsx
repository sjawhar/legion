import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { ArtifactDetails, ReferencedBy } from "./ArtifactDetails";

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

function renderDetails(target: Artifact) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ArtifactDetails artifact={target} />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("ArtifactDetails compares selected blob versions side by side", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...artifact,
    referenced_by: [],
  });

  try {
    const view = renderDetails(artifact);

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
    view.unmount();
  } finally {
    getArtifact.mockRestore();
  }
});

test("ArtifactDetails renders the artifact's own inbound references from the detail query", async () => {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...artifact,
    referenced_by: [
      {
        excerpt: "See dispatch://CORE-1/artifact/diagram-png before deciding.",
        id: "comment-1",
        issue_key: "CORE-1",
        kind: "comment",
        project: "CORE",
      },
    ],
  });

  try {
    const view = renderDetails(artifact);

    const referencedBy = await screen.findByRole("region", { name: "Referenced by" });
    expect(referencedBy.textContent).toContain("Comment · CORE-1");
    view.unmount();
  } finally {
    getArtifact.mockRestore();
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
