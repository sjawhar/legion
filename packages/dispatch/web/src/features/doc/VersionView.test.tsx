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
  const view = render(
    <QueryClientProvider client={queryClient}>
      <DocumentRuntime.Provider value={runtime.runtime}>
        <VersionView
          artifactId="artifact-1"
          asks={[]}
          blockSchema={blockSchema}
          createdAt="2026-09-09T00:00:00Z"
          highlight={highlight}
          owner={undefined}
          version={1}
        />
      </DocumentRuntime.Provider>
    </QueryClientProvider>
  );
  return { ...runtime, blockSchema, view };
}

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
    expect(within(view.container).getByRole("status").textContent).toBe(
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
        throw new Error("text directives (:name{...}) are not supported");
      };
      return handle;
    },
    highlight: undefined,
    markdown: "held since 16:25Z",
  });

  try {
    await waitFor(() =>
      expect(within(view.container).getByRole("alert").textContent).toBe(
        "This version could not be rendered: text directives (:name{...}) are not supported"
      )
    );
    expect(editors[0]?.destroyed).toBe(true);
    expect(within(view.container).getByRole("heading").textContent).toMatch(/^Version 1 · /);
  } finally {
    view.unmount();
  }
});
