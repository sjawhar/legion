import { beforeAll, expect, test } from "bun:test";
import type { ChangeDecorations, DocumentJSON } from "@legion/proof-editor";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { Node as ProseMirrorNode } from "prosemirror-model";

import { fakeDocumentRuntime, markedText } from "../../__tests__/document-runtime";
import type { DisplayTree } from "../../api/types";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import { changeMarksAgainst } from "./change-marks";
import type { CreateEditor, EditorHandle } from "./editor";
import { embedHighlight } from "./highlight";
import { DocumentRuntime } from "./runtime";
import { VersionView } from "./VersionView";

// Every row seeds the version the way the server answers for it: its markdown and the reading
// Go made of that text. The page's own engine reads it into the same document, so the engine is
// loaded before the first row builds a tree.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function renderVersionView({
  changeBaseline,
  createEditor,
  highlight,
  markdown,
  markdownTree,
}: {
  changeBaseline?: DisplayTree;
  createEditor?: (inner: CreateEditor) => CreateEditor;
  highlight: { by: string; id: string; quote: string } | undefined;
  markdown: string;
  /** The server's reading of `markdown`, when a row needs the one it built itself; the page's
   *  own reading of that text otherwise. */
  markdownTree?: DisplayTree;
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
    markdown_tree: markdownTree ?? treeOf(markdown),
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
          changeBaseline={changeBaseline}
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
    expect(editors[0]).toMatchObject({ focused: ["comment-1"], readOnly: true });
    // The editor was handed the server's tree with the quote marked in it, not markdown.
    expect(editors[0]?.markdown).toBeUndefined();
    expect(editors[0]?.root.textContent).toBe("Read the current specification");
    expect(markedText(editors[0]?.documentJSON, "proofComment")).toEqual(["current"]);
    expect(editors[0]?.options.awareness).toBeNull();
  } finally {
    view.unmount();
  }
});

test("VersionView renders a typed callout with the fetched block schema", async () => {
  const markdown = ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n';
  const markdownTree = treeOf(markdown);
  const { blockSchema, editors, view } = renderVersionView({
    highlight: undefined,
    markdown,
    markdownTree,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(blockSchema);
    expect(editors[0]?.documentJSON).toEqual(markdownTree as DocumentJSON);
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
    expect(markedText(editors[0]?.documentJSON, "proofComment")).toEqual([]);
    const status = await within(view.container).findByRole("status");
    expect(status.textContent).toBe(
      "Text changed. The selected range no longer exists in this document."
    );
  } finally {
    view.unmount();
  }
});

test("VersionView shows what refused the version instead of an empty document", async () => {
  const { editors, view } = renderVersionView({
    createEditor: (inner) => async (root, options) => {
      const handle = await inner(root, options);
      handle.setDocumentJSON = () => {
        throw new Error("this version cannot be drawn");
      };
      return handle;
    },
    highlight: undefined,
    markdown: "Some version text.",
  });

  try {
    await waitFor(() =>
      expect(within(view.container).getByRole("alert").textContent).toBe(
        "This version could not be rendered: this version cannot be drawn"
      )
    );
    expect(editors[0]?.destroyed).toBe(true);
    expect(within(view.container).getByRole("heading").textContent).toMatch(/^Version 1 · /);
  } finally {
    view.unmount();
  }
});

/** The document the editor builds for `markdown`, which the component marks against its
 *  baseline: the fake editor holds no ProseMirror document of its own. */
function renderedDocument(markdown: string): ProseMirrorNode {
  const doc = loadedEngine()?.parse(markdown);
  if (doc === undefined) throw new Error(`the engine rendered nothing for "${markdown}"`);
  return doc;
}

/** The reading the server sends of a stored text: Go's own, which the page's own engine reads
 *  that text into node for node - the document the version page draws, and the baseline it
 *  marks against. */
function treeOf(markdown: string): DisplayTree {
  return renderedDocument(markdown).toJSON() as DisplayTree;
}

/** Records what the host draws over the rendered document, and gives the handle `doc`, the
 *  document the editor would hold: the fake runtime's view holds a plain state object with none
 *  of its own, and the component reads `view.state.doc` to compute the marks. The document is
 *  built by the caller, before the editor is asked for anything, so a row counting what the
 *  page parses counts no parse of the test's own. */
function recordMarks(
  doc: ProseMirrorNode,
  drawn: ChangeDecorations[]
): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, options);
    const state = handle.view.state as unknown as { doc: ProseMirrorNode };
    state.doc = doc;
    handle.setChangeDecorations = (decorations) => drawn.push(decorations);
    return handle;
  };
}

test("VersionView draws the marks its baseline computes over the version it renders", async () => {
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n\nThe end.\n";
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n\nThe old note.\n\nThe end.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderVersionView({
    changeBaseline: treeOf(baseline),
    createEditor: recordMarks(renderedDocument(markdown), drawn),
    highlight: undefined,
    markdown,
  });

  try {
    await waitFor(() => expect(drawn).toHaveLength(1));
    const marks = changeMarksAgainst(treeOf(baseline), renderedDocument(markdown));
    // The baseline changed one paragraph and lost another, so there is something to draw.
    expect(marks.text.length).toBeGreaterThan(0);
    expect(marks.widgets.length).toBeGreaterThan(0);
    expect(drawn[0]).toEqual(marks);
  } finally {
    view.unmount();
  }
});

test("VersionView draws nothing when it is given no baseline", async () => {
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n";
  const markdownTree = treeOf(markdown);
  const drawn: ChangeDecorations[] = [];
  const { editors, view } = renderVersionView({
    createEditor: recordMarks(renderedDocument(markdown), drawn),
    highlight: undefined,
    markdown,
    markdownTree,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    await Bun.sleep(20);
    expect(editors[0]?.documentJSON).toEqual(markdownTree as DocumentJSON);
    expect(drawn).toEqual([]);
  } finally {
    view.unmount();
  }
});

// What the browser spec counts on each page it opens (`e2e/parse-counting.ts`): the two entries
// a reading page can parse markdown through. One is the shared read-only engine's `parse`, which
// every display surface reads with; the other is the editor handle's `setMarkdown`, its markdown
// import, which builds a `ParserState` of its own. This row counts the same two around the
// render of a past version, with its quote highlighted and its change marks drawn - everything
// the page does with the version's text - and both must stay at zero: the version is drawn from
// the tree the server sent.
test("VersionView draws the version's tree and parses nothing, on either engine", async () => {
  const engine = loadedEngine();
  if (engine === undefined) throw new Error("the row needs the shared engine loaded");
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n";
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  // Everything the test itself reads is read before the counting starts.
  const markdownTree = treeOf(markdown);
  const changeBaseline = treeOf(baseline);
  const rendered = renderedDocument(markdown);
  const drawn: ChangeDecorations[] = [];
  const parses: string[] = [];
  const counted = engine as { parse: typeof engine.parse };
  const enginesParse = counted.parse;
  counted.parse = (...args) => {
    parses.push("display");
    return enginesParse.apply(engine, args);
  };

  const { editors, view } = renderVersionView({
    changeBaseline,
    createEditor: (inner) => async (root, options) => {
      const handle = await recordMarks(rendered, drawn)(inner)(root, options);
      handle.setMarkdown = () => {
        parses.push("editor");
      };
      return handle;
    },
    highlight: { by: "user:alice", id: "comment-1", quote: "canary" },
    markdown,
    markdownTree,
  });

  try {
    await waitFor(() => expect(drawn).toHaveLength(1));
    expect(parses).toEqual([]);
    expect(editors[0]?.documentJSON).toEqual(
      embedHighlight(markdownTree as DocumentJSON, {
        by: "user:alice",
        id: "comment-1",
        quote: "canary",
      }) as DocumentJSON
    );
    expect(markedText(editors[0]?.documentJSON, "proofComment")).toEqual(["canary"]);
    expect(editors[0]?.focused).toEqual(["comment-1"]);
  } finally {
    counted.parse = enginesParse;
    view.unmount();
  }
});
