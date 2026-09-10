import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";

import type { Artifact } from "../../api/types";
import { MarginProvider, useMargin } from "../margin/Margin";
import { DocEditor } from "./DocEditor";

const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "b52b3d6d-eace-486c-98f9-4963e25d5bf8",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

let receivedSelection:
  | {
      artifact?: string;
      artifactId?: string;
      canSuggest?: boolean;
      occurrence?: number;
      quote?: string;
    }
  | undefined;

function SelectionObserver() {
  const { selection } = useMargin();
  useEffect(() => {
    receivedSelection = selection;
  }, [selection]);
  return null;
}

function createQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function renderDocEditor({
  artifact: document = artifact,
  highlight,
  isClosed = false,
  queryClient = createQueryClient(),
}: {
  artifact?: Artifact;
  highlight?: { from: number; to: number };
  isClosed?: boolean;
  queryClient?: QueryClient;
} = {}) {
  queryClient.setQueryData(["artifact", document.id], document);
  return render(
    <QueryClientProvider client={queryClient}>
      <MarginProvider>
        <DocEditor artifact={document} highlight={highlight} isClosed={isClosed} />
        <SelectionObserver />
      </MarginProvider>
    </QueryClientProvider>
  );
}

test("DocEditor renders the live text and reports a quote selection", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "The quick brown fox",
    version: 1,
  });
  receivedSelection = undefined;
  const view = renderDocEditor({ queryClient });

  try {
    const article = await within(view.container).findByRole("article");
    expect(article.textContent).toContain("The quick brown fox");
    const text = article.querySelector("p")?.firstChild;
    if (text?.nodeType !== Node.TEXT_NODE) {
      throw new Error("DocEditor test fixture needs a text paragraph.");
    }
    const range = document.createRange();
    range.setStart(text, 10);
    range.setEnd(text, 15);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    fireEvent.mouseUp(article);

    await waitFor(() =>
      expect(receivedSelection).toMatchObject({
        artifact: artifact.id,
        artifactId: artifact.id,
        canSuggest: true,
        occurrence: 0,
        quote: "brown",
      })
    );
  } finally {
    view.unmount();
  }
});

test("DocEditor shows the closed-issue notice with the rendered document", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Closed document",
    version: 1,
  });
  const view = renderDocEditor({ isClosed: true, queryClient });

  try {
    expect(
      within(view.container).getByText("This issue is closed. Its document is read-only.")
    ).not.toBeNull();
    expect((await within(view.container).findByRole("article")).textContent).toContain(
      "Closed document"
    );
  } finally {
    view.unmount();
  }
});

test("DocEditor compares a selected version with the current live text", async () => {
  const version = {
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-09-09T00:00:00Z",
    named: false,
    number: 1,
    summary: null,
  };
  const artifactWithVersion = { ...artifact, versions: [version] };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Use Postgres",
    version: 2,
  });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    markdown: "Use SQLite",
    version: 1,
  });
  const view = renderDocEditor({ artifact: artifactWithVersion, queryClient });

  try {
    const documentEditor = within(view.container);
    await waitFor(() =>
      expect(documentEditor.getByRole("article").textContent).toContain("Use Postgres")
    );
    fireEvent.change(documentEditor.getByLabelText("Version"), { target: { value: "1" } });
    await documentEditor.findByTestId("version-view");
    fireEvent.click(documentEditor.getByRole("button", { name: "Diff vs current" }));
    await waitFor(() => {
      const diff = documentEditor.getByTestId("version-diff");
      expect(diff.querySelector("del")?.textContent).toContain("SQLite");
      expect(diff.querySelector("ins")?.textContent).toContain("Postgres");
    });
  } finally {
    view.unmount();
  }
});

test("DocEditor resets version mode and diff state for a different artifact", async () => {
  const version = {
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-09-09T00:00:00Z",
    named: true,
    number: 1,
    summary: "Initial version",
  };
  const firstArtifact = { ...artifact, versions: [version] };
  const secondArtifact = {
    ...artifact,
    id: "a710eb6a-06dc-4dcc-bb7e-325f13e5cddf",
    name: "second.md",
    slug: "second",
    versions: [],
  };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", firstArtifact.id, "text"], {
    markdown: "First version",
    version: 1,
  });
  queryClient.setQueryData(["artifact", firstArtifact.id, "version", 1], {
    markdown: "First version",
    version: 1,
  });
  queryClient.setQueryData(["artifact", secondArtifact.id, "text"], {
    markdown: "Second version",
    version: 1,
  });
  const rendered = renderDocEditor({ artifact: firstArtifact, queryClient });

  try {
    const editor = within(rendered.container);
    fireEvent.change(editor.getByLabelText("Version"), { target: { value: "1" } });
    await editor.findByTestId("version-view");
    fireEvent.click(editor.getByRole("button", { name: "Diff vs current" }));

    rendered.rerender(
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <DocEditor artifact={secondArtifact} isClosed={false} />
          <SelectionObserver />
        </MarginProvider>
      </QueryClientProvider>
    );

    expect((editor.getByLabelText("Version") as HTMLSelectElement).value).toBe("");
    expect(editor.queryByTestId("version-view")).toBeNull();
    expect(editor.queryByTestId("version-diff")).toBeNull();
    await waitFor(() =>
      expect(editor.getByRole("article").textContent).toContain("Second version")
    );
  } finally {
    rendered.unmount();
  }
});

test("DocEditor renders a deep-linked historical range", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Read the current specification",
    version: 1,
  });
  const rendered = renderDocEditor({
    highlight: { from: 9, to: 16 },
    queryClient,
  });

  try {
    await waitFor(() =>
      expect(rendered.container.querySelector("mark.dispatch-anchor-history")?.textContent).toBe(
        "current"
      )
    );
  } finally {
    rendered.unmount();
  }
});

test("DocEditor shows the current document when Version changes back to Current", async () => {
  const version = {
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-09-09T00:00:00Z",
    named: false,
    number: 1,
    summary: null,
  };
  const artifactWithVersion = { ...artifact, versions: [version] };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Current document",
    version: 2,
  });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    markdown: "Historical document",
    version: 1,
  });
  const rendered = renderDocEditor({ artifact: artifactWithVersion, queryClient });

  try {
    const documentEditor = within(rendered.container);
    fireEvent.change(documentEditor.getByLabelText("Version"), { target: { value: "1" } });
    await documentEditor.findByTestId("version-view");
    fireEvent.change(documentEditor.getByLabelText("Version"), { target: { value: "" } });
    await waitFor(() =>
      expect(documentEditor.getByRole("article").textContent).toContain("Current document")
    );
  } finally {
    rendered.unmount();
  }
});
