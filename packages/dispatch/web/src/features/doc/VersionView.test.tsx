import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import type { CreateEditor } from "./editor";
import { DocumentRuntime } from "./runtime";
import { VersionView } from "./VersionView";

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function renderVersionView({
  createEditor,
  highlight,
  markdown,
}: {
  createEditor?: (inner: CreateEditor) => CreateEditor;
  highlight: { by: string; id: string; quote: string } | undefined;
  markdown: string;
}) {
  const runtime = fakeDocumentRuntime();
  if (createEditor !== undefined) {
    runtime.runtime = {
      ...runtime.runtime,
      createEditor: createEditor(runtime.runtime.createEditor),
    };
  }
  const queryClient = createQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["artifact", "artifact-1", "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown,
    named: false,
    number: 1,
    summary: null,
  });
  const blockSchema = runtime.runtime.blockSchema;
  if (blockSchema === undefined) {
    throw new Error("VersionView test runtime must provide a block schema.");
  }
  const tree = (shown: { by: string; id: string; quote: string } | undefined) => (
    <QueryClientProvider client={queryClient}>
      <DocumentRuntime.Provider value={runtime.runtime}>
        <VersionView
          artifactId="artifact-1"
          asks={[]}
          blockSchema={blockSchema}
          createdAt="2026-09-09T00:00:00Z"
          highlight={shown}
          owner={undefined}
          version={1}
        />
      </DocumentRuntime.Provider>
    </QueryClientProvider>
  );
  const view = render(tree(highlight));
  /** Renders the view again with `shown`, as its parent does on every render of its own. */
  const rerender = (shown: { by: string; id: string; quote: string } | undefined) =>
    view.rerender(tree(shown));
  return { ...runtime, blockSchema, rerender, view };
}

// The parent builds `highlight` afresh on every render. An editor rebuilt for each such render
// collapsed the page and scrolled again, and its last scroll could run before the last editor had
// laid out, leaving the reader at the top of the page; a browser row catches that only sometimes.
test("VersionView keeps its editor when the parent renders again with an equal highlight, and builds a new one for another comment", async () => {
  const markdown = "Read the current specification and the older note";
  const { editors, rerender, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "current" },
    markdown,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    rerender({ by: "user:alice", id: "comment-1", quote: "current" });
    rerender({ by: "user:alice", id: "comment-1", quote: "current" });
    await Bun.sleep(20);
    expect(editors).toHaveLength(1);
    expect(editors[0]?.destroyed).toBe(false);
    expect(editors[0]?.focused).toEqual(["comment-1"]);

    rerender({ by: "user:bob", id: "comment-2", quote: "older" });
    await waitFor(() => expect(editors).toHaveLength(2));
    expect(editors[0]?.destroyed).toBe(true);
    expect(editors[1]?.focused).toEqual(["comment-2"]);
  } finally {
    view.unmount();
  }
});

test("VersionView renders the version read-only with the highlighted quote focused", async () => {
  const { editors, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "current" },
    markdown: "Read the current specification",
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(
      within(view.container).getByRole("region", { name: "Document version 1" })
    ).toBeDefined();
    expect(editors[0]).toMatchObject({
      focused: ["comment-1"],
      markdown:
        'Read the <span data-proof="comment" data-id="comment-1" data-by="user:alice">current</span> specification',
      readOnly: true,
    });
    expect(editors[0]?.options.awareness).toBeNull();
  } finally {
    view.unmount();
  }
});

test("VersionView renders a typed callout with the fetched block schema", async () => {
  const markdown = ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n';
  const { blockSchema, editors, view } = renderVersionView({
    highlight: undefined,
    markdown,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(blockSchema);
    expect(editors[0]?.markdown).toBe(markdown);
  } finally {
    view.unmount();
  }
});

test("VersionView reports an ambiguous or missing quote as changed text", async () => {
  const { editors, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "o" },
    markdown: "o o",
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.markdown).toBe("o o");
    const status = await within(view.container).findByRole("status");
    expect(status.textContent).toBe(
      "Text changed. The selected range no longer exists in this document."
    );
  } finally {
    view.unmount();
  }
});

test("VersionView shows the parser's error instead of an empty document when a version cannot be rendered", async () => {
  const { editors, view } = renderVersionView({
    createEditor: (inner) => async (root, options) => {
      const handle = await inner(root, options);
      handle.setMarkdown = () => {
        throw new Error("this version cannot be parsed");
      };
      return handle;
    },
    highlight: undefined,
    markdown: "Some version text.",
  });

  try {
    await waitFor(() =>
      expect(within(view.container).getByRole("alert").textContent).toBe(
        "This version could not be rendered: this version cannot be parsed"
      )
    );
    expect(editors[0]?.destroyed).toBe(true);
    expect(within(view.container).getByRole("heading").textContent).toMatch(/^Version 1 · /);
  } finally {
    view.unmount();
  }
});
