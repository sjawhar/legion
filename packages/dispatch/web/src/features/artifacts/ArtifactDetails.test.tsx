import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { ArtifactDetails } from "./ArtifactDetails";

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
  });

  try {
    const view = renderDetails(artifact);

    // The comparison is a deliberate action: nothing to pick from until it is opened.
    expect(screen.queryByLabelText("Compare diagram.png from")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show comparison" }));
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

test("ArtifactDetails links each inbound source to the record that cites it", async () => {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [
      {
        created_at: "2026-09-09T01:00:00Z",
        direction: "in",
        excerpt: { text: "See dispatch://CORE-1/artifact/diagram-png before deciding." },
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
      {
        created_at: "2026-09-09T00:30:00Z",
        direction: "in",
        excerpt: { text: "Source artifact" },
        kind: "mentions",
        node: {
          id: "artifact-source",
          issue_key: "CORE-1",
          kind: "artifact",
          project: "CORE",
          ref: "dispatch://CORE-1/artifact/source-doc",
        },
        source_seq: 2,
      },
    ],
    node: {
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "artifact",
      project: "CORE",
      ref: "dispatch://CORE-1/artifact/diagram-png",
    },
  });

  try {
    const view = renderDetails(artifact);

    expect(
      (await screen.findByRole("link", { name: "Comment · CORE-1" })).getAttribute("href")
    ).toBe("/issues/CORE-1/comments/comment-1");
    // A citing document navigates to that document, never to its owning issue.
    expect(screen.getByRole("link", { name: "Artifact · CORE-1" }).getAttribute("href")).toBe(
      "/issues/CORE-1/artifacts/source-doc"
    );
    view.unmount();
  } finally {
    getReferences.mockRestore();
  }
});

test("ArtifactDetails groups a source's edges into one card and expands its clamped excerpt", async () => {
  const long = `${"A cited passage that runs well past the clamp. ".repeat(8)}End of block.`;
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [
      {
        created_at: "2026-09-09T01:00:00Z",
        direction: "in",
        excerpt: { block_id: "b-1", text: long },
        kind: "mentions",
        node: {
          id: "artifact-source",
          issue_key: "CORE-1",
          kind: "artifact",
          project: "CORE",
          ref: "dispatch://CORE-1/artifact/source-doc",
        },
        source_seq: 3,
      },
      {
        created_at: "2026-09-09T00:30:00Z",
        direction: "in",
        excerpt: { text: "spec.md" },
        kind: "anchored_to",
        node: {
          id: "artifact-source",
          issue_key: "CORE-1",
          kind: "artifact",
          project: "CORE",
          ref: "dispatch://CORE-1/artifact/source-doc",
        },
        source_seq: null,
      },
    ],
    node: {
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "artifact",
      project: "CORE",
      ref: "dispatch://CORE-1/artifact/diagram-png",
    },
  });

  try {
    const view = renderDetails(artifact);

    const references = await screen.findByRole("region", { name: "Referenced by" });
    // One source, two edges: one card carrying both, not two cards repeating the source.
    expect(within(references).getAllByRole("link", { name: "Artifact · CORE-1" })).toHaveLength(1);
    expect(within(references).getByText("Anchored item")).toBeTruthy();

    const excerpt = within(references).getByText(long);
    expect(excerpt.className).toContain("line-clamp-3");
    fireEvent.click(within(references).getByRole("button", { name: "Show more" }));
    expect(within(references).getByText(long).className).not.toContain("line-clamp-3");
    expect(within(references).getByRole("button", { name: "Show less" })).toBeTruthy();
    view.unmount();
  } finally {
    getReferences.mockRestore();
  }
});

test("ArtifactDetails fetches and renders a document diff only after the reader opens the comparison", async () => {
  const doc: Artifact = {
    ...artifact,
    id: "artifact-doc",
    kind: "doc",
    name: "spec.md",
    slug: "spec-md",
    versions: artifact.versions.map(
      ({ mime: _mime, sha256: _sha256, size: _size, ...version }) => version
    ),
  };
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(doc);
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockImplementation(
    async (_id, version) => ({
      authors: [],
      created_at: "2026-09-14T00:00:00Z",
      markdown: version === 1 ? "Before\n" : "After\n",
      named: false,
      number: version,
      summary: null,
    })
  );

  try {
    const view = renderDetails(doc);
    await screen.findByRole("button", { name: "Show comparison" });
    expect(getArtifactVersion).not.toHaveBeenCalled();
    expect(screen.queryByLabelText("Compare spec.md from")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Show comparison" }));
    expect(await screen.findByLabelText("Compare spec.md from")).not.toBeNull();
    await screen.findByText("After", { exact: false });
    expect(getArtifactVersion).toHaveBeenCalledTimes(2);

    fireEvent.click(screen.getByRole("button", { name: "Hide comparison" }));
    expect(screen.queryByLabelText("Compare spec.md from")).toBeNull();
    view.unmount();
  } finally {
    getArtifact.mockRestore();
    getArtifactVersion.mockRestore();
  }
});

// A block id is a uuid only by default: a markdown author sets their own with `:::ask{#id}`, so
// one carrying a space or a percent reaches the deep link. Every other `#b-` producer encodes it
// and `ProofDocument` decodes it, so an unencoded fragment misses the block or throws there.
test("ArtifactDetails encodes an author-set block id in the source deep link", async () => {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [
      {
        created_at: "2026-09-09T01:00:00Z",
        direction: "in",
        excerpt: { block_id: "two words%", text: "A decision block cites this." },
        kind: "mentions",
        node: {
          id: "artifact-source",
          issue_key: "CORE-1",
          kind: "artifact",
          project: "CORE",
          ref: "dispatch://CORE-1/artifact/source-doc",
        },
        source_seq: 3,
      },
    ],
    node: {
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "artifact",
      project: "CORE",
      ref: "dispatch://CORE-1/artifact/diagram-png",
    },
  });

  try {
    const view = renderDetails(artifact);
    const references = await screen.findByRole("region", { name: "Referenced by" });
    const source = within(references).getByRole("link", { name: "Artifact · CORE-1" });
    expect(source.getAttribute("href")).toContain("#b-two%20words%25");
    view.unmount();
  } finally {
    getReferences.mockRestore();
  }
});
