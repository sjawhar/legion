import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { colorForLogin } from "./connection";
import { ProofDocument } from "./ProofDocument";
import { DocumentRuntime } from "./runtime";

const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function renderProofDocument({
  document = artifact,
  isClosed = false,
  queryClient = createQueryClient(),
  version,
}: {
  document?: Artifact;
  isClosed?: boolean;
  queryClient?: QueryClient;
  version?: number;
} = {}) {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const onVersionChange = (_version: number | null) => {};
  const renderDocument = (next: { isClosed?: boolean; version?: number } = {}) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <ProofDocument
            artifact={document}
            highlight={undefined}
            isClosed={next.isClosed ?? isClosed}
            issueKey="CORE-1"
            onVersionChange={onVersionChange}
            user={{ kind: "user", login: "alice" }}
            version={next.version ?? version}
          />
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(renderDocument());
  return {
    ...fake,
    rerender(next: { isClosed?: boolean; version?: number }) {
      view.rerender(renderDocument(next));
    },
    view,
  };
}

test("ProofDocument creates the editor on the synced document as the signed-in user", async () => {
  const { connections, editors, sync, view } = renderProofDocument();

  try {
    expect(within(view.container).getByRole("status").textContent).toBe(
      "Connecting to the document…"
    );
    expect(connections).toHaveLength(1);
    sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options).toMatchObject({
      awareness: connections[0]?.awareness,
      heatMapMode: "hidden",
      readOnly: false,
      user: { color: colorForLogin("alice"), name: "alice" },
      ydoc: connections[0]?.doc,
    });
    expect(editors[0]?.root.textContent).toContain("The live document");
    expect(editors[0]?.root.getAttribute("aria-label")).toBe("Document editor");
    expect(editors[0]?.root.getAttribute("aria-multiline")).toBe("true");
    expect(editors[0]?.root.getAttribute("role")).toBe("textbox");
  } finally {
    view.unmount();
  }
});

test("ProofDocument reflects connection status and read-only state", async () => {
  const { editors, rerender, status, sync, view } = renderProofDocument();

  try {
    sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => status("connected"));
    expect(within(view.container).getByRole("status").textContent).toBe("connected");
    act(() => status("offline"));
    expect(within(view.container).getByRole("status").textContent).toBe("offline");
    rerender({ isClosed: true });
    expect(editors[0]?.readOnly).toBe(true);
    expect(within(view.container).getByRole("article").getAttribute("data-read-only")).toBe("true");
    expect(
      within(view.container).getByText("This issue is closed. Its document is read-only.")
    ).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("ProofDocument keeps the live editor mounted while a version is shown and destroys it once", async () => {
  const versionedArtifact = {
    ...artifact,
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T00:00:00Z",
        named: false,
        number: 1,
        summary: null,
      },
    ],
  };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "Version one",
    named: false,
    number: 1,
    summary: null,
  });
  const { connections, editors, rerender, sync, view } = renderProofDocument({
    document: versionedArtifact,
    queryClient,
  });

  try {
    sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    rerender({ version: 1 });
    await within(view.container).findByTestId("version-view");
    expect(
      view.container
        .querySelector("section[aria-label='Document editor'] article")
        ?.hasAttribute("hidden")
    ).toBe(true);
    expect(editors[0]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
  expect(editors[0]?.destroyed).toBe(true);
  expect(connections[0]?.destroyed).toBe(true);
});

test("ProofDocument compares a selected version with the current settled text", async () => {
  const document = {
    ...artifact,
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T00:00:00Z",
        named: false,
        number: 1,
        summary: null,
      },
    ],
  };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Use Postgres",
    version: 2,
  });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "Use SQLite",
    named: false,
    number: 1,
    summary: null,
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const { sync, view } = renderProofDocument({ document, queryClient, version: 1 });

  try {
    sync();
    fireEvent.click(within(view.container).getByRole("button", { name: "Diff vs current" }));
    await waitFor(() => {
      const diff = within(view.container).getByTestId("version-diff");
      expect(diff.querySelector("del")?.textContent).toContain("SQLite");
      expect(diff.querySelector("ins")?.textContent).toContain("Postgres");
    });
  } finally {
    view.unmount();
  }
});

test("ProofDocument shows the current document when Version changes back to Current", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "Historical document",
    named: false,
    number: 1,
    summary: null,
  });
  const { rerender, sync, view } = renderProofDocument({ queryClient });

  try {
    sync();
    rerender({ version: 1 });
    await within(view.container).findByTestId("version-view");
    rerender({ version: undefined });
    await waitFor(() =>
      expect(within(view.container).getByRole("article").textContent).toContain("The live document")
    );
  } finally {
    view.unmount();
  }
});

test("ProofDocument links to the current version when a historic version is unavailable", async () => {
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockRejectedValue(
    new Error("version not found")
  );
  const { view } = renderProofDocument({ version: 9 });

  try {
    await within(view.container).findByText("No version 9 of spec.md.");
    expect(
      within(view.container)
        .getByRole("link", { name: "View current version" })
        .getAttribute("href")
    ).toBe("/issues/CORE-1/spec");
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});
