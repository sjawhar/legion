import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import { Composer } from "./Composer";

test("Composer uploads dropped files and inserts their references", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "file",
      name: "notes.md",
      primary: false,
      slug: "notes-md",
      versions: [],
    },
    version: {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-09T00:00:00Z",
      mime: "text/markdown",
      named: false,
      number: 1,
      sha256: "sha",
      size: 1,
      summary: null,
    },
  });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <Composer
          anchor={{ artifact: "document-1", from: 0, quote: "text", to: 4 }}
          issueKey="CORE-1"
          kind="comment"
          onClose={() => {}}
        />
      </QueryClientProvider>
    );

    const textarea = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.drop(textarea, {
      dataTransfer: {
        files: [new File(["# Notes"], "notes.md", { type: "text/markdown" })],
      },
    });

    await waitFor(() => expect(textarea.value).toBe("dispatch://CORE-1/artifact/notes-md"));
  } finally {
    uploadArtifact.mockRestore();
  }
});
