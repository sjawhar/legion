import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import { Composer } from "./Composer";

test("Composer refreshes issue artifacts after a pasted image uploads", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const invalidateQueries = spyOn(queryClient, "invalidateQueries");
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "image",
      name: "diagram.png",
      primary: false,
      slug: "diagram-png",
      versions: [],
    },
    version: {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-09T00:00:00Z",
      mime: "image/png",
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

    fireEvent.paste(screen.getByLabelText("Comment"), {
      clipboardData: {
        files: [new File(["image"], "diagram.png", { type: "image/png" })],
      },
    });

    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    expect(invalidateQueries).toHaveBeenCalledWith({ queryKey: ["artifacts", "CORE-1"] });
    expect(invalidateQueries).toHaveBeenCalledWith({ queryKey: ["issue", "CORE-1"] });
  } finally {
    invalidateQueries.mockRestore();
    uploadArtifact.mockRestore();
  }
});
