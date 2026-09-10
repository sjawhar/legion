import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { DocumentRuntime } from "./runtime";
import { VersionView } from "./VersionView";

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function renderVersionView({
  highlight,
  markdown,
}: {
  highlight: { by: string; id: string; quote: string } | undefined;
  markdown: string;
}) {
  const runtime = fakeDocumentRuntime();
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
  const view = render(
    <QueryClientProvider client={queryClient}>
      <DocumentRuntime.Provider value={runtime.runtime}>
        <VersionView
          artifactId="artifact-1"
          createdAt="2026-09-09T00:00:00Z"
          highlight={highlight}
          version={1}
        />
      </DocumentRuntime.Provider>
    </QueryClientProvider>
  );
  return { ...runtime, view };
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
