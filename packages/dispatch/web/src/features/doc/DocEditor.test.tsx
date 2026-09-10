import { expect, spyOn, test } from "bun:test";
import { EditorView } from "@codemirror/view";
import { HocuspocusProvider } from "@hocuspocus/provider";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { Artifact } from "../../api/types";
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

class WebSocketStub {
  binaryType = "arraybuffer";
  identifier = 0;
  readyState = 0;

  addEventListener(..._args: unknown[]): void {}

  close(): void {
    this.readyState = 3;
  }

  removeEventListener(..._args: unknown[]): void {}

  send(..._args: unknown[]): void {}
}

test("DocEditor destroys its room provider when it unmounts", () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const destroyProvider = spyOn(HocuspocusProvider.prototype, "destroy");
  const destroyEditor = spyOn(EditorView.prototype, "destroy");

  try {
    const { unmount } = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifact} isClosed={false} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    unmount();
    expect(destroyProvider).toHaveBeenCalledTimes(1);
    expect(destroyEditor).toHaveBeenCalledTimes(1);
  } finally {
    destroyProvider.mockRestore();
    destroyEditor.mockRestore();
    globalThis.WebSocket = originalWebSocket;
  }
});

test("DocEditor makes a closed document visibly read-only", () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  try {
    const { container, unmount } = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifact} isClosed={true} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    expect(screen.getByText("This issue is closed. Its document is read-only.")).not.toBeNull();
    expect(container.querySelector(".cm-content")?.getAttribute("contenteditable")).toBe("false");
    unmount();
  } finally {
    globalThis.WebSocket = originalWebSocket;
  }
});

test("DocEditor preserves the room when the issue closes", () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const providerConfiguration = spyOn(HocuspocusProvider.prototype, "setConfiguration");
  const destroy = spyOn(HocuspocusProvider.prototype, "destroy");

  try {
    const { rerender, unmount } = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifact} isClosed={false} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    rerender(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifact} isClosed={true} user={{ login: "alice" }} />
      </QueryClientProvider>
    );

    expect(providerConfiguration).toHaveBeenCalledTimes(1);
    expect(destroy).not.toHaveBeenCalled();
    unmount();
  } finally {
    providerConfiguration.mockRestore();
    destroy.mockRestore();
    globalThis.WebSocket = originalWebSocket;
  }
});

test("DocEditor uses an empty synchronized document in preview and version diffs", async () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const version = {
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-09-09T00:00:00Z",
    named: false,
    number: 1,
    summary: null,
  };
  const artifactWithVersion = { ...artifact, versions: [version] };
  let provider: HocuspocusProvider | undefined;
  const originalSetConfiguration = HocuspocusProvider.prototype.setConfiguration;
  const captureProvider = spyOn(
    HocuspocusProvider.prototype,
    "setConfiguration"
  ).mockImplementation(function (this: HocuspocusProvider, configuration) {
    provider = this;
    return originalSetConfiguration.call(this, configuration);
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["artifact", artifact.id], artifactWithVersion);
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Use SQLite",
    version: 1,
  });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    markdown: "Use SQLite",
    version: 1,
  });
  let unmount: (() => void) | undefined;

  try {
    const rendered = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifactWithVersion} isClosed={false} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    unmount = rendered.unmount;
    const documentEditor = within(rendered.container);
    await waitFor(() => expect(provider).toBeDefined());
    const documentProvider = provider;
    if (documentProvider === undefined) {
      throw new Error("DocEditor did not create a document provider.");
    }
    const ytext = documentProvider.document.getText("content");
    act(() => {
      ytext.insert(0, "Use SQLite");
      documentProvider.synced = true;
      ytext.delete(0, ytext.length);
    });

    expect(documentEditor.getByRole("button", { name: "Edit" })).not.toBeNull();
    await waitFor(() => expect(documentEditor.queryByText("Use SQLite")).toBeNull());

    fireEvent.change(documentEditor.getByLabelText("Version"), { target: { value: "1" } });
    await documentEditor.findByTestId("version-view");
    fireEvent.click(documentEditor.getByRole("button", { name: "Diff vs current" }));
    await waitFor(() => {
      const diff = documentEditor.getByTestId("version-diff");
      expect(diff.querySelector("del")?.textContent).toContain("Use SQLite");
      expect(diff.querySelector("ins")).toBeNull();
    });
  } finally {
    unmount?.();
    captureProvider.mockRestore();
    globalThis.WebSocket = originalWebSocket;
  }
});

test("DocEditor resets version mode and diff state for a different artifact", async () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
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
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["artifact", firstArtifact.id], firstArtifact);
  queryClient.setQueryData(["artifact", firstArtifact.id, "version", 1], {
    markdown: "First version",
    version: 1,
  });

  try {
    const rendered = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={firstArtifact} isClosed={false} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    const editor = within(rendered.container);
    fireEvent.change(editor.getByLabelText("Version"), { target: { value: "1" } });
    await editor.findByTestId("version-view");
    fireEvent.click(editor.getByRole("button", { name: "Diff vs current" }));

    rendered.rerender(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={secondArtifact} isClosed={false} user={{ login: "alice" }} />
      </QueryClientProvider>
    );

    expect((editor.getByLabelText("Version") as HTMLSelectElement).value).toBe("");
    expect(editor.getByRole("button", { name: "Edit" })).not.toBeNull();
    expect(editor.queryByTestId("version-view")).toBeNull();
    expect(editor.queryByTestId("version-diff")).toBeNull();
    rendered.unmount();
  } finally {
    globalThis.WebSocket = originalWebSocket;
  }
});

test("DocEditor renders a deep-linked historical range in preview", async () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["artifact", artifact.id], artifact);
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Read the current specification",
    version: 1,
  });

  try {
    const rendered = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor
          artifact={artifact}
          highlight={{ from: 9, to: 16 }}
          isClosed={false}
          user={{ login: "alice" }}
        />
      </QueryClientProvider>
    );

    await waitFor(() =>
      expect(rendered.container.querySelector("mark.dispatch-anchor-history")?.textContent).toBe(
        "current"
      )
    );
    rendered.unmount();
  } finally {
    globalThis.WebSocket = originalWebSocket;
  }
});

test("DocEditor keeps preview mode when Version changes back to Current", async () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const version = {
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-09-09T00:00:00Z",
    named: false,
    number: 1,
    summary: null,
  };
  const artifactWithVersion = { ...artifact, versions: [version] };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["artifact", artifact.id], artifactWithVersion);
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Current document",
    version: 2,
  });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    markdown: "Historical document",
    version: 1,
  });

  try {
    const rendered = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifactWithVersion} isClosed={false} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    const documentEditor = within(rendered.container);

    fireEvent.change(documentEditor.getByLabelText("Version"), { target: { value: "1" } });
    await documentEditor.findByTestId("version-view");
    fireEvent.change(documentEditor.getByLabelText("Version"), { target: { value: "" } });

    expect(documentEditor.getByRole("button", { name: "Edit" })).not.toBeNull();
    await waitFor(() =>
      expect(documentEditor.getByRole("article").textContent).toContain("Current document")
    );
    rendered.unmount();
  } finally {
    globalThis.WebSocket = originalWebSocket;
  }
});
