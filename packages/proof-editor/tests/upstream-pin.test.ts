/**
 * The pin has to carry every fix the fork's `library` line carries, and `upstream/` — the
 * declarations tsc reads in its place (AGENTS.md § The upstream boundary) — has to be what the
 * pinned sources emit. Both otherwise fail silently: nothing in this repository reads those
 * editor modules until a browser renders a document with them, and a stale declaration is a
 * shape tsc believes and the runtime does not have.
 *
 * Each fix case below is one member of that line. The proof-mark rendering fix is checked by
 * running the pinned modules: each mark rendered from the schema the headless engine builds, and
 * the marks plugin drawing a replacement in a real (happy-dom) view. The split-mark actions and
 * the overlapping record marks are checked the same way, through the marks plugin's own actions
 * on a headless state. The Dark Reader fix has no exported seam a unit test could call, so it is
 * read where it lives. A cut that loses one passes every other check in the repository.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Ctx } from "@milkdown/kit/ctx";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { EditorState, Plugin } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import {
  applyRemoteMarks,
  comment,
  deleteMark,
  marksPlugin,
  marksPluginKey,
  reject,
} from "proof-sdk-upstream/src/editor/plugins/marks";
import type { StoredMark } from "proof-sdk-upstream/src/formats/marks";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withDomWindow } from "./dom-window";
import { markedText } from "./mark-text";
import { viewDouble } from "./view-double";

const upstreamSrc = join(import.meta.dir, "..", "node_modules", "proof-sdk-upstream", "src");

test("the pinned dependency carries the Dark Reader fix", () => {
  const marks = readFileSync(join(upstreamSrc, "editor/plugins/marks.ts"), "utf8");
  expect(marks).not.toContain("const STYLES =");
  expect(marks).not.toContain("style: STYLES.compose_anchor");
  expect(marks).not.toContain("span.style.cssText = STYLES.insert");
  expect(marks).toContain("class: [cssClass, glowClass].filter(Boolean).join(' '),");

  const cursors = readFileSync(join(upstreamSrc, "editor/plugins/collab-cursors.ts"), "utf8");
  expect(cursors).not.toContain("cursorWidget.style.setProperty");
  expect(cursors).not.toContain("'data-proof-collab-selection':");
  expect(cursors).toContain("function ensureCollabColorStyles");
  expect(cursors).toContain("proof-collab-selection--");
});

test("the pinned dependency renders every proof mark with its data attributes only", async () => {
  // Upstream's attribute slices default to `{ id: {}, by: {} }` and each mark's toDOM spreads
  // them onto the span, which renders id="[object Object]" beside the real data-id.
  const { schema } = await createHeadlessProof();
  const samples: Array<[string, Record<string, string>, Record<string, string>]> = [
    [
      "proofSuggestion",
      { by: "ai:tester", id: "m-1", kind: "replace" },
      { "data-kind": "replace" },
    ],
    ["proofComment", { by: "human:tester", id: "m-2" }, {}],
    ["proofFlagged", { by: "human:tester", id: "m-3" }, {}],
    ["proofApproved", { by: "human:tester", id: "m-4" }, {}],
  ];
  for (const [name, attrs, extra] of samples) {
    const type = schema.marks[name];
    if (type === undefined) throw new Error(`the editor schema has no ${name} mark`);
    const rendered: unknown = type.spec.toDOM?.(type.create(attrs), true);
    if (!Array.isArray(rendered)) throw new Error(`${name} renders no DOM output spec`);
    const proof = name.slice("proof".length).toLowerCase();
    expect({ mark: name, attrs: rendered[1] }).toEqual({
      mark: name,
      attrs: { "data-by": attrs.by, "data-id": attrs.id, "data-proof": proof, ...extra },
    });
  }
  const authored = schema.marks.proofAuthored;
  if (authored === undefined) throw new Error("the editor schema has no proofAuthored mark");
  const rendered: unknown = authored.spec.toDOM?.(
    authored.create({ by: "human:tester", id: "m-5" }),
    true
  );
  if (!Array.isArray(rendered)) throw new Error("proofAuthored renders no DOM output spec");
  expect(rendered[1]).toEqual({
    "data-by": "human:tester",
    "data-proof": "authored",
    "data-proof-id": "m-5",
  });
});

test("the pinned dependency redraws a replacement revised on a viewer's page", async () => {
  // A viewer who cannot edit receives marks with `hydrateAnchors: false`, which never touches
  // the document. The replace-insert widget must still show the revised replacement, which it
  // does only when its key changes with the replacement.
  const { schema } = await createHeadlessProof();
  const suggestion = schema.marks.proofSuggestion;
  if (suggestion === undefined) throw new Error("the editor schema has no proofSuggestion mark");
  await withDomWindow(async (window) => {
    // `$prose` hands the ProseMirror plugin back once its Milkdown wrapper has run; the marks
    // factory ignores its context, so one that answers `$prose`'s wait and update is enough.
    const proseContext = {
      update: (_slice: unknown, updater: (plugins: unknown[]) => unknown[]) => void updater([]),
      wait: async () => undefined,
    } as unknown as Ctx;
    await marksPlugin(proseContext)();
    const doc = schema.node("doc", null, [
      schema.node("paragraph", null, [
        schema.text("The "),
        schema.text("quick brown", [
          suggestion.create({ by: "ai:tester", id: "replace-1", kind: "replace" }),
        ]),
        schema.text(" fox"),
      ]),
    ]);
    const mount = window.document.body.appendChild(window.document.createElement("div"));
    const view = new EditorView(mount as unknown as HTMLElement, {
      state: EditorState.create({ doc, plugins: [marksPlugin.plugin()], schema }),
    });
    // Older than the marks plugin's 2 s glow window, so its class does not force a redraw.
    const createdAt = new Date(Date.now() - 60_000).toISOString();
    const replacement = (content: string): Record<string, StoredMark> => ({
      "replace-1": {
        by: "ai:tester",
        content,
        createdAt,
        kind: "replace",
        quote: "quick brown",
        status: "pending",
      },
    });

    applyRemoteMarks(view, replacement("slow red"), { hydrateAnchors: false });
    expect(view.dom.querySelector(".mark-replace-insert")?.textContent).toBe("slow red");
    applyRemoteMarks(view, replacement("slow blue"), { hydrateAnchors: false });
    expect(view.dom.querySelector(".mark-replace-insert")?.textContent).toBe("slow blue");
    view.destroy();
  });
});

/** A view double whose state holds the marks plugin's metadata, which its actions read. */
function headlessMarksView(doc: ProseMirrorNode, metadata: Record<string, StoredMark>): EditorView {
  const plugin = new Plugin({
    key: marksPluginKey,
    state: {
      init: () => ({ metadata, activeMarkId: null }),
      apply: (tr, value) => {
        const meta = tr.getMeta(marksPluginKey);
        return meta?.type === "SET_METADATA" ? { ...value, metadata: meta.metadata } : value;
      },
    },
  });
  return viewDouble(EditorState.create({ doc, plugins: [plugin] })).view;
}

test("the pinned dependency acts on a split insert's own runs", async () => {
  // Alice's insert "quick brown" with Bob's insert "lazy " between its runs. Rejecting hers
  // joined her runs into one range and deleted his text too (EveryInc/proof-sdk#83).
  const { schema } = await createHeadlessProof();
  const suggestion = schema.marks.proofSuggestion;
  if (suggestion === undefined) throw new Error("the editor schema has no proofSuggestion mark");
  const insert = (id: string, by: string) => suggestion.create({ by, id, kind: "insert" });
  const createdAt = new Date(Date.now() - 60_000).toISOString();
  const stored = (by: string, content: string) =>
    ({ by, content, createdAt, kind: "insert", status: "pending" }) as StoredMark;
  const doc = schema.node("doc", null, [
    schema.node("paragraph", null, [
      schema.text("The "),
      schema.text("quick ", [insert("a", "human:alice")]),
      schema.text("lazy ", [insert("b", "human:bob")]),
      schema.text("brown", [insert("a", "human:alice")]),
      schema.text(" fox"),
    ]),
  ]);
  const view = headlessMarksView(doc, {
    a: stored("human:alice", "quick brown"),
    b: stored("human:bob", "lazy "),
  });
  expect(reject(view, "a")).toBe(true);
  expect(view.state.doc.textContent).toBe("The lazy  fox");
  expect(markedText(view.state.doc, "b")).toBe("lazy ");
});

test("the pinned dependency lets two people's comments and suggestions cover the same text", async () => {
  const { schema, parseMarkdown, serializeMarkdown } = await createHeadlessProof();
  const { proofComment, proofSuggestion, proofFlagged, proofApproved, proofAuthored } =
    schema.marks;
  if (!proofComment || !proofSuggestion || !proofFlagged || !proofApproved || !proofAuthored) {
    throw new Error("the editor schema is missing a proof mark");
  }
  expect({
    proofComment: proofComment.excludes(proofComment),
    proofSuggestion: proofSuggestion.excludes(proofSuggestion),
    proofFlagged: proofFlagged.excludes(proofFlagged),
    proofApproved: proofApproved.excludes(proofApproved),
    proofAuthored: proofAuthored.excludes(proofAuthored),
  }).toEqual({
    proofComment: false,
    proofSuggestion: false,
    proofFlagged: true,
    proofApproved: true,
    proofAuthored: true,
  });

  // Bob comments on "quick brown", Alice on "brown": Bob's comment keeps "brown", and deleting
  // Alice's removes hers only.
  const doc = schema.node("doc", null, [
    schema.node("paragraph", null, [schema.text("The quick brown fox")]),
  ]);
  const view = headlessMarksView(doc, {});
  const bob = comment(view, "quick brown", "human:bob", "Bob", { from: 5, to: 16 });
  const alice = comment(view, "brown", "human:alice", "Alice", { from: 11, to: 16 });
  expect(markedText(view.state.doc, bob.id)).toBe("quick brown");
  expect(markedText(view.state.doc, alice.id)).toBe("brown");

  // Written to markdown, the nested spans read back as both comments.
  const reparsed = parseMarkdown(serializeMarkdown(view.state.doc));
  expect(markedText(reparsed, bob.id)).toBe("quick brown");
  expect(markedText(reparsed, alice.id)).toBe("brown");

  expect(deleteMark(view, alice.id)).toBe(true);
  expect(markedText(view.state.doc, alice.id)).toBe("");
  expect(markedText(view.state.doc, bob.id)).toBe("quick brown");
});

// The timeout is explicit because the case spawns tsc over the whole upstream closure — two
// TypeScript programs — and that is the work, not a hang. Bun's 5 s default is under the cost on
// a two-core runner, where the timeout would read exactly like a stale `upstream/`.
test("upstream/ is what the pinned sources emit", async () => {
  const generator =
    await Bun.$`bun ${join(import.meta.dir, "..", "scripts", "upstream-declarations.ts")} --check`
      .cwd(join(import.meta.dir, ".."))
      .nothrow()
      .quiet();
  const output = generator.stdout.toString() + generator.stderr.toString();
  expect(output).toContain("matches the pin");
  expect(generator.exitCode).toBe(0);
}, 120_000);
