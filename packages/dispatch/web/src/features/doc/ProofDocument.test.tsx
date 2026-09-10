import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import { MemoryRouter } from "react-router-dom";

import { type FakeDocumentRuntime, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { MarginProvider, useMargin } from "../margin/Margin";
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
  fake = fakeDocumentRuntime({ text: "The live document" }),
  isClosed = false,
  queryClient = createQueryClient(),
  version,
}: {
  document?: Artifact;
  fake?: FakeDocumentRuntime;
  isClosed?: boolean;
  queryClient?: QueryClient;
  version?: number;
} = {}) {
  const margin = {
    current: undefined as
      | {
          documentBridge:
            | { focusMark(markId: string): void; setActiveMarks(markIds: string[]): void }
            | undefined;
          focusRequest: { markId: string; seq: number } | undefined;
          hoveredMarkId: string | undefined;
          markPositions: ReadonlyMap<string, number>;
          pendingCompose:
            | {
                anchor: { artifact: string; mark_id: string; quote: string };
                kind: "ask" | "comment" | "suggestion";
                seq: number;
              }
            | undefined;
          settleCompose(outcome: "saved" | "cancelled"): void;
        }
      | undefined,
  };
  const onVersionChange = (_version: number | null) => {};
  const MarginProbe = () => {
    margin.current = useMargin();
    return null;
  };
  const renderDocument = (next: { isClosed?: boolean; version?: number } = {}) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <MarginProbe />
            <ProofDocument
              artifact={document}
              highlight={undefined}
              isClosed={next.isClosed ?? isClosed}
              issueKey="CORE-1"
              onVersionChange={onVersionChange}
              user={{ kind: "user", login: "alice" }}
              version={next.version ?? version}
            />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(renderDocument());
  return {
    ...fake,
    margin,
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

test("a selection-bar action opens the margin composer for the mark and settles the library promise", async () => {
  const { editors, margin, sync, view } = renderProofDocument();

  try {
    sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const onMarkAction = editors[0]?.options.onMarkAction;
    if (onMarkAction === undefined) {
      throw new Error("The editor did not receive an onMarkAction handler.");
    }

    let comment: Promise<void> | undefined;
    act(() => {
      comment = Promise.resolve(
        onMarkAction({ from: 11, kind: "comment", markId: "m-9", quote: "brown", to: 16 })
      );
      void comment.catch(() => {});
    });
    if (comment === undefined) {
      throw new Error("The comment action did not return a promise.");
    }
    expect(margin.current?.pendingCompose).toMatchObject({
      anchor: { artifact: artifact.id, mark_id: "m-9", quote: "brown" },
      kind: "comment",
    });
    act(() => margin.current?.settleCompose("saved"));
    await expect(comment).resolves.toBeUndefined();

    let suggest: Promise<void> | undefined;
    act(() => {
      suggest = Promise.resolve(
        onMarkAction({ from: 11, kind: "suggest", markId: "m-10", quote: "brown", to: 16 })
      );
      void suggest.catch(() => {});
    });
    if (suggest === undefined) {
      throw new Error("The suggestion action did not return a promise.");
    }
    expect(margin.current?.pendingCompose?.kind).toBe("suggestion");
    act(() => margin.current?.settleCompose("saved"));
    await expect(suggest).resolves.toBeUndefined();

    let ask: Promise<void> | undefined;
    act(() => {
      ask = Promise.resolve(
        onMarkAction({ from: 11, kind: "ask", markId: "m-11", quote: "brown", to: 16 })
      );
      void ask.catch(() => {});
    });
    if (ask === undefined) {
      throw new Error("The ask action did not return a promise.");
    }
    expect(margin.current?.pendingCompose?.kind).toBe("ask");
    act(() => margin.current?.settleCompose("cancelled"));
    await expect(ask).rejects.toThrow("composer closed");

    let unsupported: unknown;
    try {
      const outcome = onMarkAction({ kind: "resolve", markId: "m-9" });
      if (outcome instanceof Promise) {
        void outcome.catch(() => {});
      }
    } catch (error) {
      unsupported = error;
    }
    expect(unsupported).toEqual(
      new Error("Dispatch renders mark threads in the margin; popover action resolve cannot fire")
    );
  } finally {
    view.unmount();
  }
});

test("a highlight click focuses its margin item and hover identifies its matching margin item", async () => {
  const { editors, margin, sync, view } = renderProofDocument();

  try {
    sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const onMarkClick = editors[0]?.options.onMarkClick;
    const onMarkHover = editors[0]?.options.onMarkHover;
    if (onMarkClick === undefined || onMarkHover === undefined) {
      throw new Error("The editor did not receive mark interaction handlers.");
    }
    const span = document.createElement("span");
    span.dataset.id = "m-9";
    editors[0]?.root.replaceChildren(span);

    act(() => onMarkClick("m-9"));
    expect(margin.current?.focusRequest?.markId).toBe("m-9");

    act(() => onMarkHover("m-9"));
    expect(margin.current?.hoveredMarkId).toBe("m-9");
    act(() => onMarkHover(null));
    expect(margin.current?.hoveredMarkId).toBeUndefined();

    act(() => margin.current?.documentBridge?.focusMark("m-9"));
    expect(editors[0]?.focused).toEqual(["m-9"]);

    act(() => margin.current?.documentBridge?.setActiveMarks(["m-9"]));
    expect(span.classList.contains("dispatch-mark-active")).toBe(true);
  } finally {
    view.unmount();
  }
});

test("the marks projection reaches the editor on sync and on every change", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const projected: { metadata: object; options: { hydrateAnchors?: boolean } | undefined }[] = [];
  const createEditor = fake.runtime.createEditor;
  fake.runtime.createEditor = async (root, options) => {
    const handle = await createEditor(root, options);
    const applyRemoteMarks = handle.applyRemoteMarks.bind(handle);
    handle.applyRemoteMarks = (metadata, applyOptions) => {
      projected.push({ metadata, options: applyOptions });
      applyRemoteMarks(metadata, applyOptions);
    };
    return handle;
  };
  const { connections, editors, sync, view } = renderProofDocument({ fake });
  const storedMark = {
    by: "user:alice",
    kind: "comment",
    replies: [],
    resolved: false,
    text: "why?",
  };

  try {
    sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => connections[0]?.doc.getMap("marks").set("c-1", storedMark));
    await waitFor(() =>
      expect(projected.at(-1)).toEqual({
        metadata: { "c-1": storedMark },
        options: { hydrateAnchors: false },
      })
    );
  } finally {
    view.unmount();
  }
});

test("mark positions are published to the margin after document changes", async () => {
  const schema = new Schema({
    marks: {
      dispatchAsk: { attrs: { by: {}, id: {} } },
      proofComment: { attrs: { by: {}, id: {} } },
    },
    nodes: {
      doc: { content: "paragraph+" },
      paragraph: { content: "inline*", group: "block" },
      text: { group: "inline" },
    },
  });
  const comment = schema.marks.proofComment.create({ by: "alice", id: "c-1" });
  const ask = schema.marks.dispatchAsk.create({ by: "bob", id: "a-1" });
  const initialDocument = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [schema.text("The "), schema.text("quick", [comment])]),
  ]);
  const changedDocument = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [schema.text("The "), schema.text("quick", [ask])]),
  ]);
  const fake = fakeDocumentRuntime({ text: "The live document" });
  let editorState: { doc: ProseMirrorNode } | undefined;
  const createEditor = fake.runtime.createEditor;
  fake.runtime.createEditor = async (root, options) => {
    const handle = await createEditor(root, options);
    editorState = handle.view.state as unknown as { doc: ProseMirrorNode };
    editorState.doc = initialDocument;
    return handle;
  };
  const { connections, editors, margin, sync, view } = renderProofDocument({ fake });

  try {
    sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(margin.current?.markPositions.get("c-1")).toBe(5));

    if (editorState === undefined) {
      throw new Error("The editor state was not captured.");
    }
    editorState.doc = changedDocument;
    act(() => connections[0]?.doc.getXmlFragment("prosemirror").delete(0, 1));
    await waitFor(() => {
      expect(margin.current?.markPositions.get("c-1")).toBeUndefined();
      expect(margin.current?.markPositions.get("a-1")).toBe(5);
    });
  } finally {
    view.unmount();
  }
});
