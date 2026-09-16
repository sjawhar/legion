import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import { MemoryRouter, useLocation } from "react-router-dom";

import { type FakeDocumentRuntime, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { api } from "../../api/client";
import type { Artifact, Ask, IssueDetails } from "../../api/types";
import { MarginProvider, useMargin } from "../margin/Margin";
import type { MarkPlacement } from "../margin/useMarginItems";
import { RefPreviewHost } from "../refs/RefPreview";
import { colorForLogin } from "./connection";
import { type DocumentToolbar, ProofDocument } from "./ProofDocument";
import { DocumentRuntime } from "./runtime";

const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

const blockSchema = {
  types: [
    {
      attributes: {
        kind: { choices: ["note", "warning"], default: "note", kind: "enum" as const },
        title: { default: "", kind: "string" as const },
      },
      content: "paragraph+" as const,
      name: "callout",
      render: "host" as const,
    },
  ],
  version: 1,
};

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function CurrentRoute() {
  const location = useLocation();
  return <div data-testid="current-route">{location.pathname}</div>;
}

function renderProofDocument({
  document = artifact,
  fake = fakeDocumentRuntime({ text: "The live document" }),
  highlightTerm,
  isClosed = false,
  queryClient = createQueryClient(),
  showDiff = false,
  version,
}: {
  document?: Artifact;
  fake?: FakeDocumentRuntime;
  highlightTerm?: string;
  isClosed?: boolean;
  queryClient?: QueryClient;
  showDiff?: boolean;
  version?: number;
} = {}) {
  const margin = {
    current: undefined as
      | {
          blockFilterId: string | undefined;
          focusBlock(blockId: string): void;
          documentBridge:
            | { focusMark(markId: string): void; setActiveMarks(markIds: string[]): void }
            | undefined;
          focusRequest: { markId: string; seq: number } | undefined;
          hoveredMarkId: string | undefined;
          markPlacements: ReadonlyMap<string, MarkPlacement>;
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
  queryClient.setQueryData(["block-schema"], blockSchema);
  const toolbar: { current: DocumentToolbar | undefined } = { current: undefined };
  const onToolbarChange = (next: DocumentToolbar) => {
    toolbar.current = next;
  };
  const onVersionChange = (_version: number | null) => {};
  const MarginProbe = () => {
    margin.current = useMargin();
    return null;
  };
  const renderDocument = (
    next: { highlightTerm?: string; isClosed?: boolean; showDiff?: boolean; version?: number } = {}
  ) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <CurrentRoute />
            <MarginProbe />
            <ProofDocument
              artifact={document}
              highlight={undefined}
              highlightTerm={next.highlightTerm ?? highlightTerm}
              isClosed={next.isClosed ?? isClosed}
              onToolbarChange={onToolbarChange}
              owner={{ key: "CORE-1", kind: "issue" }}
              onVersionChange={onVersionChange}
              showDiff={next.showDiff ?? showDiff}
              user={{ kind: "user", login: "alice" }}
              version={next.version ?? version}
            />
            <RefPreviewHost />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(renderDocument());
  return {
    ...fake,
    margin,
    rerender(next: {
      highlightTerm?: string;
      isClosed?: boolean;
      showDiff?: boolean;
      version?: number;
    }) {
      view.rerender(renderDocument(next));
    },
    // The server's sync can only follow the connection, which exists once the lazy transport
    // has resolved.
    async sync() {
      await waitFor(() => expect(fake.connections).toHaveLength(1));
      fake.sync();
    },
    toolbar,
    view,
  };
}

test("ProofDocument creates the editor on the synced document as the signed-in user", async () => {
  const { connections, editors, sync, toolbar, view } = renderProofDocument();

  try {
    expect(toolbar.current?.connection).toBe("connecting");
    await sync();
    expect(connections).toHaveLength(1);
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

test("ProofDocument focuses a block named by the document hash after the editor is ready", async () => {
  const originalHash = window.location.hash;
  window.history.replaceState(null, "", "#b-block-1");
  const { editors, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors[0]?.focusedBlocks).toEqual(["block-1"]));
  } finally {
    window.history.replaceState(null, "", originalHash || "/");
    view.unmount();
  }
});

test("ProofDocument clears the parent's toolbar bag on unmount", () => {
  const { toolbar, view } = renderProofDocument();

  expect(toolbar.current).not.toBeUndefined();
  view.unmount();
  expect(toolbar.current).toBeUndefined();
});

test("ProofDocument highlights a routed search term again after route and document changes", async () => {
  class TestHighlight {
    readonly ranges: Range[];

    constructor(...ranges: Range[]) {
      this.ranges = ranges;
    }
  }
  const styleApi = window as unknown as {
    CSS?: { highlights?: Map<string, TestHighlight> };
    Highlight?: typeof TestHighlight;
  };
  const originalCss = Object.getOwnPropertyDescriptor(styleApi, "CSS");
  const originalHighlight = Object.getOwnPropertyDescriptor(styleApi, "Highlight");
  const highlights = new Map<string, TestHighlight>();
  Object.defineProperties(styleApi, {
    CSS: { configurable: true, value: { highlights } },
    Highlight: { configurable: true, value: TestHighlight },
  });
  const { connections, editors, margin, rerender, sync, view } = renderProofDocument({
    fake: fakeDocumentRuntime({ text: "The astrolabe reads altitude." }),
    highlightTerm: "astrolabe",
  });

  try {
    await sync();
    await waitFor(() => expect(margin.current?.documentBridge).toBeDefined());
    await waitFor(() => {
      const highlight = highlights.get("dispatch-search");
      expect(highlight?.ranges.map((range) => range.toString())).toEqual(["astrolabe"]);
    });

    editors[0]?.root.replaceChildren(document.createTextNode("The sextant reads altitude."));
    rerender({ highlightTerm: "sextant" });
    await waitFor(() => {
      const highlight = highlights.get("dispatch-search");
      expect(highlight?.ranges.map((range) => range.toString())).toEqual(["sextant"]);
    });
    const routeHighlight = highlights.get("dispatch-search");

    editors[0]?.root.replaceChildren(document.createTextNode("The compass reads altitude."));
    act(() => connections[0]?.doc.getXmlFragment("prosemirror").delete(0, 1));
    await waitFor(() => {
      const highlight = highlights.get("dispatch-search");
      expect(highlight).not.toBe(routeHighlight);
      expect(highlight?.ranges).toEqual([]);
    });
  } finally {
    view.unmount();
    if (originalHighlight === undefined) {
      delete styleApi.Highlight;
    } else {
      Object.defineProperty(styleApi, "Highlight", originalHighlight);
    }
    if (originalCss === undefined) {
      delete styleApi.CSS;
    } else {
      Object.defineProperty(styleApi, "CSS", originalCss);
    }
  }
});

test("ProofDocument opens a hover card for a dispatch:// link in the live editor and navigates in-app", async () => {
  const issue: IssueDetails = {
    artifacts: [],
    children: [],
    closed_at: null,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    number: 1,
    parent: null,
    assignee: null,
    primary_artifact_id: "artifact-1",
    project: "CORE",
    route: null,
    status: "testing",
    priority: null,
    rank: "U",
    title: "Ship the release",
    open_asks: [],
    updated_at: "2026-09-09T00:00:00Z",
  };
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const { editors, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const root = editors[0]?.root;
    if (root === undefined) throw new Error("editor root missing");
    // The fake runtime never runs Markdown through the real proof-editor serializer (its
    // createEditor stub just dumps a debug string), so this stands in for what that serializer
    // now actually emits for a dispatch:// link mark: href sanitized to "", the real target
    // carried in data-dispatch-href. `doc.e2e.ts` proves the real serializer produces this shape
    // and that clicking it navigates in a genuinely live document.
    act(() => {
      root.innerHTML =
        '<p><a href="" data-dispatch-href="dispatch://CORE-1">dispatch://CORE-1</a></p>';
    });
    const anchor = root.querySelector("a");
    if (anchor === null) throw new Error("anchor missing");
    // Triggers are delegated document-wide, so an anchor inserted after mount is one too:
    // keyboard focus opens the card at once, with the target's title once fetched.
    act(() => {
      anchor.focus();
    });
    const card = await screen.findByRole("tooltip");
    await waitFor(() => expect(card.textContent).toContain("Ship the release"));
    expect(anchor.getAttribute("aria-describedby")).toBe(card.id);
    act(() => {
      anchor.blur();
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    fireEvent.click(anchor);
    expect(within(view.container).getByTestId("current-route").textContent).toBe("/issues/CORE-1");
  } finally {
    getIssue.mockRestore();
    view.unmount();
  }
});

test("ProofDocument reports connection status through the toolbar bag and enforces read-only state", async () => {
  const { editors, rerender, status, sync, toolbar, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => status("connected"));
    await waitFor(() => expect(toolbar.current?.connection).toBe("connected"));
    act(() => status("offline"));
    await waitFor(() => expect(toolbar.current?.connection).toBe("offline"));
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

test("a transport that fails to load is reported instead of connecting forever", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  fake.runtime.loadTransport = () => Promise.reject(new TypeError("Failed to fetch"));
  const { toolbar, view } = renderProofDocument({ fake });

  try {
    const alert = await within(view.container).findByRole("alert");
    expect(alert.textContent).toBe("This document could not load: Failed to fetch");
    expect(toolbar.current?.connection).toBe("failed");
  } finally {
    view.unmount();
  }
});

test("an editor that fails to load after sync is reported the same way, and later provider status stays out of the way", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  fake.runtime.createEditor = () => Promise.reject(new Error("editor chunk missing"));
  const { status, sync, toolbar, view } = renderProofDocument({ fake });

  try {
    await sync();
    const alert = await within(view.container).findByRole("alert");
    expect(alert.textContent).toBe("This document could not load: editor chunk missing");
    expect(toolbar.current?.connection).toBe("failed");
    // The live connection is still up and may reconnect; its dot must not contradict the alert.
    act(() => status("connected"));
    expect(toolbar.current?.connection).toBe("failed");
    expect(within(view.container).getByRole("alert").textContent).toContain("editor chunk missing");
  } finally {
    view.unmount();
  }
});

test("ProofDocument makes a schema-read-only admission non-editable and reloadable", async () => {
  const { admit, connections, editors, sync, view } = renderProofDocument();

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    act(() => admit(true));
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.readOnly).toBe(true);
    expect(within(view.container).getByRole("article").getAttribute("data-read-only")).toBe("true");
    expect(within(view.container).getByText("Reload to edit.")).toBeDefined();
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
    await sync();
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

test("ProofDocument compares a selected version with the current settled text when showDiff is true", async () => {
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
  const { sync, view } = renderProofDocument({ document, queryClient, showDiff: true, version: 1 });

  try {
    await sync();
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
    await sync();
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

test("the block reference gutter filters the margin and focuses its block", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(
    ["artifact", artifact.id, "blocks"],
    [
      {
        from: 0,
        id: "block-1",
        references: { asks: 1, comments: 1 },
        to: 17,
        type: "paragraph",
      },
    ]
  );
  const { editors, margin, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    fireEvent.click(
      await within(view.container).findByRole("button", { name: "2 references on block" })
    );
    expect(editors[0]?.focusedBlocks).toEqual(["block-1"]);
    expect(margin.current?.blockFilterId).toBe("block-1");
  } finally {
    view.unmount();
  }
});

test("a margin block focus request pulses the active block", async () => {
  const { editors, margin, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => margin.current?.focusBlock("block-1"));
    await waitFor(() => expect(editors[0]?.focusedBlocks).toContain("block-1"));
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
    await sync();
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
    await sync();
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
    await sync();
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

test("mark placements are published to the margin after document changes", async () => {
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
    handle.markOffsets = () =>
      new Map([
        ["c-1", 40],
        ["a-1", 72],
      ]);
    return handle;
  };
  const { connections, editors, margin, sync, view } = renderProofDocument({ fake });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() =>
      expect(margin.current?.markPlacements.get("c-1")).toEqual({ pos: 5, top: 40 })
    );

    if (editorState === undefined) {
      throw new Error("The editor state was not captured.");
    }
    editorState.doc = changedDocument;
    act(() => connections[0]?.doc.getXmlFragment("prosemirror").delete(0, 1));
    await waitFor(() => {
      expect(margin.current?.markPlacements.get("c-1")).toBeUndefined();
      expect(margin.current?.markPlacements.get("a-1")).toEqual({ pos: 5, top: 72 });
    });
  } finally {
    view.unmount();
  }
});

// ---------------------------------------------------------------------------------------------
// Decision (`:::ask`) blocks: the wiring from editor node views to the React card and back to
// the answer route. The card itself is covered in AskBlockCard.test.tsx.
// ---------------------------------------------------------------------------------------------

const askSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    bullet_list: { content: "list_item+", group: "block" },
    list_item: { content: "paragraph+" },
    ask: {
      attrs: {
        answer: { default: undefined },
        answered_at: { default: undefined },
        answered_by: { default: undefined },
        blockId: { default: null },
        invalid: { default: undefined },
        multiple: { default: false },
        selected: { default: undefined },
        state: { default: "open" },
        urgency: { default: "med" },
      },
      content: "paragraph+ bullet_list?",
      group: "block",
    },
  },
});

function askNode(attrs: Record<string, unknown>): ProseMirrorNode {
  return askSchema.node("ask", { blockId: "b-1", ...attrs }, [
    askSchema.node("paragraph", undefined, [askSchema.text("Should we ship?")]),
    askSchema.node("bullet_list", undefined, [
      askSchema.node("list_item", undefined, [
        askSchema.node("paragraph", undefined, [askSchema.text("Ship: Release it")]),
      ]),
      askSchema.node("list_item", undefined, [
        askSchema.node("paragraph", undefined, [askSchema.text("Hold")]),
      ]),
    ]),
  ]);
}

const blockAsk: Ask = {
  anchor: null,
  answer: null,
  author: { id: "01a086ad", kind: "session", origin: { session_title: "Architect for CORE-1" } },
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "b-1",
  created_at: new Date(Date.now() - 3 * 60_000).toISOString(),
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ description: "Release it", label: "Ship" }, { label: "Hold" }],
  question: "Should we ship?",
  state: "open",
  urgency: "high",
};

test("ProofDocument hands its decision blocks the indexed ask; the hosted card answers through the ask route and the document's reads refresh", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["asks", "CORE-1"], [blockAsk]);
  queryClient.setQueryData(["artifact", "artifact-1", "text"], { markdown: "stale" });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: blockAsk,
    edits: [],
    followers: [],
    replies: [],
  });
  const answerAsk = spyOn(api, "answerAsk").mockResolvedValue({
    ...blockAsk,
    answer: { at: new Date().toISOString(), selected: ["Ship"], text: "Go.", user: "alice" },
    state: "answered",
  });
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");
    // The fake editor records the node views the host installs; constructing the `ask` one is
    // what the real editor does for every ask node in the document.
    const nodeViews = editor.viewProps.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    let askView: { dom: HTMLElement; destroy(): void } | undefined;
    act(() => {
      askView = construct(askNode({ urgency: "high" }));
      if (askView !== undefined) editor.root.append(askView.dom);
    });
    const section = editor.root.querySelector("section[data-dispatch-ask-block]");
    if (section === null) throw new Error("the ask block shell was not mounted");
    const block = within(section as HTMLElement);
    // The block hosts the shared ask card for its indexed row: it names the asker and reads
    // the thread the Inbox card would.
    const hosted = await block.findByRole("article", { name: "Urgency: High" });
    expect(within(hosted).getByText("Architect for CORE-1")).toBeDefined();
    await waitFor(() => expect(getAsk).toHaveBeenCalledWith("ask-1"));
    fireEvent.click(await block.findByRole("radio", { name: "Ship Release it" }));
    fireEvent.click(block.getByRole("button", { name: "Add a note or answer in your own words" }));
    fireEvent.change(block.getByLabelText("Your answer"), { target: { value: "Go." } });
    fireEvent.click(block.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(answerAsk.mock.calls[0]).toEqual([
      "ask-1",
      { expected_edited_at: null, selected: ["Ship"], text: "Go." },
    ]);
    // The server writes the outcome into the block, so the document's own reads go stale.
    await waitFor(() =>
      expect(queryClient.getQueryState(["artifact", "artifact-1", "text"])?.isInvalidated).toBe(
        true
      )
    );
    act(() => askView?.destroy());
    await waitFor(() => expect(section.querySelector("[data-dispatch-ask-pill]")).toBeNull());
  } finally {
    answerAsk.mockRestore();
    getAsk.mockRestore();
    view.unmount();
  }
});

test("after Ask back on a decision block, the hosted card's turn label follows the refetched issue asks to the agent's turn", async () => {
  const openAsk: Ask = { ...blockAsk, waiting_on: "human" };
  const handedOver: Ask = { ...blockAsk, waiting_on: "agent" };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["asks", "CORE-1"], [openAsk]);
  // The owner's list is what the block reads; after the clarification the server says the
  // asker holds the turn.
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([handedOver]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: openAsk,
    edits: [],
    followers: [],
    replies: [],
  });
  const createComment = spyOn(api, "createComment").mockResolvedValue({
    anchor: null,
    ask_id: "ask-1",
    author: { id: "alice", kind: "user" },
    body: "Ship where?",
    created_at: new Date().toISOString(),
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: "agent",
  });
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");
    const nodeViews = editor.viewProps.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    act(() => {
      const askView = construct(askNode({ urgency: "high" }));
      editor.root.append(askView.dom);
    });
    const section = editor.root.querySelector("section[data-dispatch-ask-block]");
    if (section === null) throw new Error("the ask block shell was not mounted");
    const block = within(section as HTMLElement);
    expect((await block.findByTestId("turn-ask-1")).textContent).toBe("Waiting on you");
    fireEvent.click(block.getByRole("button", { name: "Add a note or answer in your own words" }));
    fireEvent.change(block.getByLabelText("Your answer"), { target: { value: "Ship where?" } });
    fireEvent.click(block.getByRole("button", { name: "Ask back" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    // The clarification invalidates the owner's asks; the refetch hands the block the new turn.
    await waitFor(() => expect(listIssueAsks).toHaveBeenCalled());
    await waitFor(() =>
      expect(block.getByTestId("turn-ask-1").textContent).toBe("Waiting on Architect for CORE-1")
    );
  } finally {
    createComment.mockRestore();
    getAsk.mockRestore();
    listIssueAsks.mockRestore();
    view.unmount();
  }
});
