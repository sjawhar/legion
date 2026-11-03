import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import type { ChangeDecorations } from "@legion/proof-editor";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { ApiError, api } from "../../api/client";
import type { Artifact, AskSnooze, InboxRow, IssueDetails, Version } from "../../api/types";
import { CHANGED_BLOCK_CLASS, changeMarksAgainst } from "../doc/change-marks";
import type { CreateEditor, EditorHandle } from "../doc/editor";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import { installInboxApiMocks, issueAsk } from "./inbox-fixture";
import { ReadingPane } from "./ReadingPane";

installInboxApiMocks();

/** The pane waits this long after the last change of the selected question before it opens
 *  another document (`DOCUMENT_SETTLE_MS` in `ReadingPane.tsx`). */
const SETTLE_MS = 200;

let getArtifact: Mock<typeof api.getArtifact>;
let getArtifactText: Mock<typeof api.getArtifactText>;
let getIssue: Mock<typeof api.getIssue>;

beforeEach(() => {
  getArtifact = spyOn(api, "getArtifact").mockRejectedValue(new Error("no document seeded"));
  getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "",
    version: null,
  });
  // The pane reads one field of the issue: whether it is closed, which it hands the document.
  getIssue = spyOn(api, "getIssue").mockResolvedValue({
    closed_at: null,
  } as unknown as IssueDetails);
});

afterEach(() => {
  getArtifact.mockRestore();
  getArtifactText.mockRestore();
  getIssue.mockRestore();
});

function version(number: number): Version {
  return {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-10-02T00:00:00Z",
    named: false,
    number,
    summary: null,
  };
}

function document(overrides: Partial<Artifact> = {}): Artifact {
  return {
    created_at: "2026-10-01T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "doc",
    name: "spec.md",
    primary: true,
    project: "CORE",
    slug: "spec",
    versions: [],
    ...overrides,
  };
}

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** Every read the pane and the document make of one artifact, answered from the cache under the
 *  document page's own keys, so a test's render reaches the network for nothing. */
function seedDocument(
  client: QueryClient,
  artifact: Artifact,
  versions: Readonly<Record<number, string>> = {}
): void {
  client.setQueryData(["artifact", artifact.id], artifact);
  client.setQueryData(["artifact", artifact.id, "text"], { markdown: "", version: null });
  client.setQueryData(["artifact", artifact.id, "blocks"], []);
  client.setQueryData(["artifact", artifact.id, "asks"], []);
  if (artifact.issue_key !== null) {
    client.setQueryData(["asks", artifact.issue_key], []);
  }
  for (const [number, markdown] of Object.entries(versions)) {
    client.setQueryData(["artifact", artifact.id, "version", Number(number)], {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-10-02T00:00:00Z",
      markdown,
      named: false,
      number: Number(number),
      summary: null,
    });
  }
}

/** The document the headless engine builds for `markdown`: what the editor would hold, which is
 *  what the version marks its baseline against. */
function renderedDocument(markdown: string): ProseMirrorNode {
  const parsed = loadedEngine()?.parse(markdown);
  if (parsed === undefined) throw new Error(`the engine rendered nothing for "${markdown}"`);
  return parsed;
}

/** Records what the version draws over its rendered document, and gives the fake handle the
 *  document the real editor would hold: the fake view's state carries none (`VersionView.test`).
 *  With `paintMarks`, it also puts a marked block into the editor's root where the version drew
 *  any, as the real editor's decorations would: the head reads the drawn document to decide
 *  whether there is a change to step to. */
function recordMarks(
  markdown: string,
  drawn: ChangeDecorations[],
  { paintMarks = false }: { paintMarks?: boolean } = {}
): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, options);
    const state = handle.view.state as unknown as { doc: ProseMirrorNode };
    state.doc = renderedDocument(markdown);
    handle.setChangeDecorations = (decorations) => {
      drawn.push(decorations);
      if (!paintMarks || decorations.text.length === 0) return;
      const block = globalThis.document.createElement("p");
      block.className = CHANGED_BLOCK_CLASS;
      block.textContent = "The plan ships behind a canary today.";
      root.append(block);
    };
    return handle;
  };
}

/** Gives the fake editor the blocks a real one would draw, each stamped with its block id as
 *  `@legion/proof-editor` stamps them: the pane reads its section off the drawn document. */
function drawBlocks(blocks: string): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, options);
    root.innerHTML = blocks;
    return handle;
  };
}

function renderPane({
  ask,
  createEditor,
  seed = () => {},
}: {
  ask: InboxRow;
  createEditor?: (inner: CreateEditor) => CreateEditor;
  seed?: (client: QueryClient) => void;
}) {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  /** Each document the column opened a room for, in the order it opened them. */
  const opened: string[] = [];
  const runtime = {
    ...fake.runtime,
    createEditor:
      createEditor === undefined
        ? fake.runtime.createEditor
        : createEditor(fake.runtime.createEditor),
    loadTransport: async () => {
      const connect = await fake.runtime.loadTransport();
      return (artifactId: string, callbacks: Parameters<typeof connect>[1]) => {
        opened.push(artifactId);
        return connect(artifactId, callbacks);
      };
    },
  };
  const client = createQueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  seed(client);
  const answered: string[] = [];
  /** Every registration the head's Snooze made, in order: `[askId, live]`. */
  const snoozeLiveness: (readonly [string, boolean])[] = [];
  const tree = (row: InboxRow) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={runtime}>
          <MarginProvider>
            <ReadingPane
              ask={row}
              onAnswered={(id) => answered.push(id)}
              onSnoozeLive={(askId, live) => snoozeLiveness.push([askId, live])}
              viewer="alice"
            />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(ask));
  return {
    answered,
    client,
    opened,
    rerender: (row: InboxRow) => view.rerender(tree(row)),
    runtime: fake,
    snoozeLiveness,
    view,
  };
}

function decisionAsk(overrides: Partial<InboxRow> = {}): InboxRow {
  return issueAsk({
    block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
    block_id: "ship_it",
    question: "Ship behind a flag?",
    ...overrides,
  });
}

/** The approval request for `spec.md` at version 4; `approved` is the approval it follows, which
 *  a first request has none of. */
function approvalAsk(approved?: {
  at: string;
  by: { id: string; kind: "user" };
  version: number;
}): InboxRow {
  return issueAsk({
    approval: {
      approved,
      artifact_id: "artifact-1",
      name: "spec.md",
      requested_version: 4,
      version: 4,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    kind: "approval",
    question: "Approve spec.md at version 4?",
  });
}

const EARLIER_APPROVAL = {
  at: "2026-10-01T00:00:00Z",
  by: { id: "alice", kind: "user" as const },
  version: 3,
};

test("a decision block draws the document in pane mode, with Skip to the answer at the head", async () => {
  const { opened, runtime, view } = renderPane({
    ask: decisionAsk(),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    expect(opened).toEqual(["artifact-1"]);
    runtime.sync();
    await waitFor(() => expect(runtime.editors).toHaveLength(1));
    // What `surface="pane"` means where the editor is built: no presence, and no edits.
    expect(runtime.editors[0]?.options).toMatchObject({ awareness: null, readOnly: true });
    expect(await screen.findByRole("button", { name: "Skip to the answer" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("a decision block's column marks the section from its heading down to the block", async () => {
  const { runtime, view } = renderPane({
    ask: decisionAsk(),
    createEditor: drawBlocks(
      '<h1 data-block-id="title">Plan</h1><h2 data-block-id="rollout">Rollout</h2>' +
        '<p data-block-id="context">How it goes out.</p>' +
        '<section data-block-id="ship_it">Ship behind a flag?</section>'
    ),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    expect(await screen.findByTestId("pane-section-bar")).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("a decision block before any heading sits in no section, so no bar is drawn", async () => {
  const { runtime, view } = renderPane({
    ask: decisionAsk(),
    createEditor: drawBlocks(
      '<p data-block-id="context">How it goes out.</p>' +
        '<section data-block-id="ship_it">Ship behind a flag?</section>' +
        '<h2 data-block-id="rollout">Rollout</h2>'
    ),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    await waitFor(() => expect(runtime.editors).toHaveLength(1));
    await waitFor(() =>
      expect(screen.getByTestId("inbox-pane").textContent).toContain("Ship behind a flag?")
    );
    expect(screen.queryByTestId("pane-section-bar")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("an approval draws the latest version marked against the version it follows", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n\nThe old note.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting", version: 3 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByRole("button", { name: "Approve" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Request changes" })).toBeDefined();
    // The latest version, which is the version Approve approves.
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(drawn).toHaveLength(1));
    const marks = changeMarksAgainst(baseline, renderedDocument(latest));
    expect(marks.text.length).toBeGreaterThan(0);
    expect(drawn[0]).toEqual(marks);
  } finally {
    view.unmount();
  }
});

test("an approval of a document nobody has approved marks nothing and offers no Next change", async () => {
  await warmMarkdownRenderer();
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(),
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting" },
          versions: [version(4)],
        }),
        { 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(screen.queryByText("Loading version…")).toBeNull());
    expect(drawn).toEqual([]);
    expect(screen.queryByRole("button", { name: "Next change" })).toBeNull();
  } finally {
    view.unmount();
  }
});

// "Next change" steps the blocks the version marked. The marks are drawn only once the headless
// markdown engine has warmed; an engine that never warms, or one that refuses the baseline,
// leaves `changeMarksAgainst` marking nothing (`change-marks.ts`), and the button then had
// nothing to step and did nothing. These two rows pin the offer on the marks that were drawn
// rather than on a baseline that merely loaded.
test("an approval whose version drew marks offers Next change", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    createEditor: recordMarks(latest, drawn, { paintMarks: true }),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting", version: 3 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByRole("button", { name: "Next change" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("an approval whose baseline loaded but whose marks never drew offers no Next change", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    // The marks are computed and handed over, and nothing of them reaches the drawn document.
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting", version: 3 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(drawn).toHaveLength(1));
    expect(drawn[0]?.text.length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: "Next change" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("an approved document shows the approval it follows in place of the actions, and its version", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "approved", version: 4 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.getByText(/Approved at version 3 on/)).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("an anchored question is today's card", async () => {
  const { opened, view } = renderPane({
    ask: issueAsk({
      anchor: {
        artifact_id: "artifact-1",
        block_id: "ship_it",
        mark_id: "mark-1",
        orphaned: false,
        quote: "behind a flag",
        version: 4,
      },
      anchor_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    }),
  });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(opened).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("a question in no document is today's card", async () => {
  const { opened, view } = renderPane({ ask: issueAsk() });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(opened).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("the head's Snooze registers its write, so the list keeps the row the column is on", async () => {
  const held = Promise.withResolvers<AskSnooze>();
  const snoozeAsk = spyOn(api, "snoozeAsk").mockImplementation(() => held.promise);
  const { snoozeLiveness, view } = renderPane({ ask: issueAsk() });

  try {
    fireEvent.change(await screen.findByLabelText("Snooze CORE-1: Which approach?"), {
      target: { value: "tomorrow" },
    });
    // The optimistic snooze folds the row into Later and the column follows the list, so a
    // head that kept its write to itself would be unmounted with the row it wrote about.
    await waitFor(() => expect(snoozeLiveness).toEqual([["ask-a", true]]));
  } finally {
    held.resolve({ snoozed_until: "2026-10-31T09:00:00Z" });
    view.unmount();
    snoozeAsk.mockRestore();
  }
});

test("a project document's head names the project and the document, and opens it at the section", async () => {
  const { runtime, view } = renderPane({
    ask: decisionAsk({
      document: { name: "Design notes", project: "CORE", slug: "design-notes" },
      issue: undefined,
      issue_key: null,
    }),
    createEditor: drawBlocks(
      '<h2 data-block-id="rollout">Rollout</h2>' +
        '<section data-block-id="ship_it">Ship behind a flag?</section>'
    ),
    seed: (client) =>
      seedDocument(
        client,
        document({ issue_key: null, name: "Design notes", primary: false, slug: "design-notes" })
      ),
  });

  try {
    expect(await screen.findByText("CORE · Design notes")).toBeDefined();
    // Neither applies to a project document: it has no priority and nobody is assigned to it.
    expect(screen.queryByRole("combobox", { name: /^Priority of/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Assign / })).toBeNull();
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    await waitFor(() =>
      expect(screen.getByRole("link", { name: "Open page" }).getAttribute("href")).toBe(
        "/projects/CORE/documents/design-notes#b-rollout"
      )
    );
  } finally {
    view.unmount();
  }
});

test("a document that cannot be read falls back to the card, with Open page at the head", async () => {
  getArtifact.mockRejectedValue(new Error("DOCUMENT_UNLOADABLE"));
  const { opened, view } = renderPane({ ask: decisionAsk() });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(screen.getByRole("link", { name: "Open page" })).toBeDefined();
    // Never the document page's own repair or rebuild prompt: the column opened no document.
    expect(opened).toEqual([]);
    expect(screen.queryByText(/Rebuild from the latest version/)).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a document the editor is refused admission to leaves the column on the card, with no prompt", async () => {
  // The column's own read of the text was answered from the cache (seeded below) and the read
  // the document makes for its own admission is the one the server refuses. Two things then say
  // the document cannot be opened: the document's report (`onUnavailable`, which
  // `ProofDocument.admission.test.tsx` covers on its own) and the error that refusal leaves on
  // the text query both columns share. Either way the column ends on the card and draws neither
  // of the page's prompts, which is what this test pins.
  getArtifactText.mockRejectedValue(
    new ApiError(409, { code: "DOC_SCHEMA", error: "This document is outside the Proof schema." })
  );
  const { opened, view } = renderPane({
    ask: decisionAsk(),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(screen.getByRole("link", { name: "Open page" })).toBeDefined();
    expect(screen.queryByText(/Upload markdown to replace and repair this document/)).toBeNull();
    expect(screen.queryByText(/Rebuild from the latest version/)).toBeNull();
    expect(screen.queryByText(/This document is outside the Proof schema/)).toBeNull();
    // The refusal is what closed the editor: no room was ever opened.
    expect(opened).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("a selection changed twice inside the settle opens the second document alone", async () => {
  const { opened, rerender, runtime, view } = renderPane({
    ask: decisionAsk({ id: "ask-1" }),
    seed: (client) => {
      seedDocument(client, document());
      seedDocument(client, document({ id: "artifact-2", name: "notes.md", slug: "notes" }));
    },
  });

  try {
    rerender(
      decisionAsk({
        block_artifact: { id: "artifact-2", primary: false, slug: "notes" },
        block_id: "later",
        id: "ask-2",
      })
    );
    await waitFor(() => expect(opened).toEqual(["artifact-2"]));
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    // Long past the settle, the first selection has still opened nothing.
    await Bun.sleep(SETTLE_MS * 2);
    expect(opened).toEqual(["artifact-2"]);
  } finally {
    view.unmount();
  }
});
