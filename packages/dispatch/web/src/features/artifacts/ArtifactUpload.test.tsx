import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { ArtifactUploadRow, useArtifactUpload } from "./ArtifactUpload";

function TestHarness(): ReactNode {
  const upload = useArtifactUpload({ issue: "CORE-1" });
  return <ArtifactUploadRow upload={upload} />;
}

function renderHarness() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <TestHarness />
    </QueryClientProvider>
  );
}

test("choosing a file stages it behind a summary field instead of uploading immediately", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact");
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    fireEvent.change(screen.getByLabelText("Upload artifact"), { target: { files: [file] } });

    expect(await screen.findByText("notes.md")).not.toBeNull();
    expect(screen.getByLabelText("Summary (optional)")).not.toBeNull();
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

test("confirming the staged file uploads it with the typed summary", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: {
      created_at: "2026-09-11T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "doc",
      name: "notes.md",
      primary: false,
      project: "CORE",
      slug: "notes-md",
      versions: [],
    },
    version: {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-11T00:00:00Z",
      named: false,
      number: 1,
      summary: "First draft",
    },
  });
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    fireEvent.change(screen.getByLabelText("Upload artifact"), { target: { files: [file] } });
    await screen.findByText("notes.md");
    fireEvent.change(screen.getByLabelText("Summary (optional)"), {
      target: { value: "First draft" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));

    await screen.findByText("Upload");
    expect(uploadArtifact).toHaveBeenCalledTimes(1);
    const [, input] = uploadArtifact.mock.calls[0] ?? [];
    expect(input?.name).toBe("notes.md");
    expect(input?.summary).toBe("First draft");
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

test("cancel discards the staged file without uploading", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact");
  const view = renderHarness();

  try {
    const file = new File(["content"], "notes.md", { type: "text/markdown" });
    fireEvent.change(screen.getByLabelText("Upload artifact"), { target: { files: [file] } });
    await screen.findByText("notes.md");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByText("notes.md")).toBeNull();
    expect(screen.getByRole("button", { name: "Upload" })).not.toBeNull();
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});
