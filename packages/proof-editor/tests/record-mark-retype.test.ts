import { expect, test } from "bun:test";
import type { Ctx } from "@milkdown/kit/ctx";
import type { Node as ProseMirrorNode, Schema } from "@milkdown/kit/prose/model";
import { EditorState, type Plugin } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import { Window } from "happy-dom";
import {
  comment,
  getMarks,
  marksPlugin,
  marksPluginKey,
} from "proof-sdk-upstream/src/editor/plugins/marks";
import { createAskMark } from "../src/dispatch-marks";
import { createHeadlessProof } from "../src/lib-headless.js";
import { findRecordMark, removeRecordMark, retypeMark } from "../src/record-mark-retype";

const BY = "alice";
// "quick brown" inside the paragraph "The quick brown fox" (the paragraph opens at 0, text at 1).
const RANGE = { from: 5, to: 16 };

function marksInDoc(view: EditorView) {
  const marks: { type: string; id: unknown; from: number; to: number; kind?: unknown }[] = [];
  view.state.doc.descendants((node, pos) => {
    if (!node.isText) return true;
    for (const mark of node.marks) {
      marks.push({
        type: mark.type.name,
        id: mark.attrs.id,
        from: pos,
        to: pos + node.nodeSize,
        kind: mark.attrs.kind,
      });
    }
    return true;
  });
  return marks;
}

function sentence(schema: Schema): ProseMirrorNode {
  return schema.node("doc", null, [
    schema.node("paragraph", null, [schema.text("The quick brown fox")]),
  ]);
}

/** A real EditorView with the upstream marks plugin, the way tests/upstream-pin.test.ts builds
 *  one: the creators and `normalizeMetadata` need the plugin's state. */
async function withEditor(
  run: (view: EditorView) => void,
  {
    doc = sentence,
    plugins = [],
  }: { doc?: (schema: Schema) => ProseMirrorNode; plugins?: Plugin[] } = {}
): Promise<void> {
  const { schema } = await createHeadlessProof();
  const window = new Window();
  const previous = (["document", "window"] as const).map(
    (key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)] as const
  );
  Object.assign(globalThis, { document: window.document, window });
  try {
    const proseContext = {
      update: (_slice: unknown, updater: (plugins: unknown[]) => unknown[]) => void updater([]),
      wait: async () => undefined,
    } as unknown as Ctx;
    await marksPlugin(proseContext)();
    const mount = window.document.body.appendChild(window.document.createElement("div"));
    const view = new EditorView(mount as unknown as HTMLElement, {
      state: EditorState.create({
        doc: doc(schema),
        plugins: [marksPlugin.plugin(), ...plugins],
        schema,
      }),
    });
    try {
      run(view);
    } finally {
      view.destroy();
    }
  } finally {
    for (const [key, descriptor] of previous) {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, key);
      else Object.defineProperty(globalThis, key, descriptor);
    }
    await window.happyDOM.close();
  }
}

test("retypeMark replaces the mark with one of the new kind over the same text, under a new id", async () => {
  await withEditor((view) => {
    const start = comment(view, "quick brown", BY, "", RANGE);

    const ask = retypeMark(view, start.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(ask.quote).toBe("quick brown");
    expect(ask.markId).not.toBe(start.id);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "dispatchAsk", id: ask.markId, from: 5, to: 16 }),
    ]);
    expect(findRecordMark(view.state.doc, ask.markId)).toEqual({
      range: RANGE,
      type: "dispatchAsk",
    });

    const suggest = retypeMark(view, ask.markId, "suggest", BY);
    if ("refused" in suggest) throw new Error(`the suggest retype was refused: ${suggest.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofSuggestion",
        id: suggest.markId,
        from: 5,
        to: 16,
        kind: "replace",
      }),
    ]);
    expect(getMarks(view.state).map((mark) => ({ id: mark.id, kind: mark.kind }))).toEqual([
      { id: suggest.markId, kind: "replace" },
    ]);

    const back = retypeMark(view, suggest.markId, "comment", BY);
    if ("refused" in back) throw new Error(`the comment retype was refused: ${back.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: back.markId, from: 5, to: 16 }),
    ]);
    // The suggestion's metadata went with its spans: the marks plugin normalises its metadata on
    // every document change, so no stale entry is left for it to re-synthesise as a pending
    // suggestion.
    expect(Object.keys(marksPluginKey.getState(view.state)?.metadata ?? {})).toEqual([back.markId]);
  });
});

test("retypeMark refuses, changing nothing, a mark the document does not hold and a suggestion upstream will not write", async () => {
  await withEditor((view) => {
    const start = comment(view, "quick brown", BY, "", RANGE);
    const before = marksInDoc(view);
    expect(retypeMark(view, "never", "ask", BY)).toEqual({ refused: "missing" });
    expect(marksInDoc(view)).toEqual(before);
    expect(retypeMark(view, start.id, "comment", BY)).toEqual({
      markId: start.id,
      quote: "quick brown",
    });
    expect(marksInDoc(view)).toEqual(before);
    // "ick" inside "quick": upstream refuses a suggestion that starts or ends mid-word.
    const midWord = comment(view, "ick", BY, "", { from: 7, to: 10 });
    const withMidWord = marksInDoc(view);
    expect(retypeMark(view, midWord.id, "suggest", BY)).toEqual({ refused: "unmarkable" });
    expect(marksInDoc(view)).toEqual(withMidWord);
  });
  await withEditor(
    (view) => {
      // table > header row > header("alpha"), header("beta"): "alpha" is 4..9, "beta" is 13..17.
      const across = comment(view, "alpha\nbeta", BY, "", { from: 4, to: 17 });
      const before = marksInDoc(view);
      expect(before.map((mark) => mark.type)).toEqual(["proofComment", "proofComment"]);
      expect(retypeMark(view, across.id, "suggest", BY)).toEqual({ refused: "unmarkable" });
      expect(marksInDoc(view)).toEqual(before);
      const ask = retypeMark(view, across.id, "ask", BY);
      if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
      expect(marksInDoc(view).map((mark) => mark.type)).toEqual(["dispatchAsk", "dispatchAsk"]);
    },
    {
      doc: (schema) =>
        schema.node("doc", null, [
          schema.node("table", null, [
            schema.node("table_header_row", null, [
              schema.node("table_header", null, [
                schema.node("paragraph", null, [schema.text("alpha")]),
              ]),
              schema.node("table_header", null, [
                schema.node("paragraph", null, [schema.text("beta")]),
              ]),
            ]),
            schema.node("table_row", null, [
              schema.node("table_cell", null, [schema.node("paragraph", null, [schema.text("x")])]),
              schema.node("table_cell", null, [schema.node("paragraph", null, [schema.text("y")])]),
            ]),
          ]),
        ]),
    }
  );
});

test("retypeMark refuses a kind whose mark another record already holds over part of the text", async () => {
  await withEditor((view) => {
    // Bob's recorded comment covers "quick brown"; Alice asks about "brown" inside it.
    const bob = comment(view, "quick brown", "bob", "", RANGE);
    const alice = createAskMark(view, { from: 11, to: 16 }, BY);
    if (alice === null) throw new Error("the ask mark was not written");
    const before = marksInDoc(view);
    expect(before).toEqual([
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 5, to: 11 }),
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 11, to: 16 }),
      expect.objectContaining({ type: "dispatchAsk", id: alice.id, from: 11, to: 16 }),
    ]);
    // A comment of Alice's over "brown" would cut "brown" out of Bob's comment: refused, and
    // Bob's spans are exactly as they were.
    expect(retypeMark(view, alice.id, "comment", BY)).toEqual({ refused: "overlaps" });
    expect(marksInDoc(view)).toEqual(before);
    // A suggestion over the same text displaces no comment, so it goes through; and back to an
    // ask, leaving Bob's comment whole.
    const suggest = retypeMark(view, alice.id, "suggest", BY);
    if ("refused" in suggest) throw new Error(`the suggest retype was refused: ${suggest.refused}`);
    const back = retypeMark(view, suggest.markId, "ask", BY);
    if ("refused" in back) throw new Error(`the ask retype was refused: ${back.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 5, to: 11 }),
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 11, to: 16 }),
      expect.objectContaining({ type: "dispatchAsk", id: back.markId, from: 11, to: 16 }),
    ]);
    expect(findRecordMark(view.state.doc, bob.id)).toEqual({ range: RANGE, type: "proofComment" });
    // The control for the refusal: the creator itself does cut Bob's anchor when asked to write
    // a comment over "brown" - a mark type excludes itself (LEGION-458).
    comment(view, "brown", BY, "", { from: 11, to: 16 });
    expect(findRecordMark(view.state.doc, bob.id)).toEqual({
      range: { from: 5, to: 11 },
      type: "proofComment",
    });
  });
});

test("removeRecordMark removes a provisional comment the unified marks system cannot see", async () => {
  await withEditor((view) => {
    // Upstream's `deleteMark` answers false here: a comment with no body is dropped by
    // buildAnchorMarks, so only a span-by-span removal reaches it.
    const start = comment(view, "quick brown", BY, "", RANGE);
    expect(removeRecordMark(view, start.id)).toBe(true);
    expect(marksInDoc(view)).toEqual([]);
    expect(removeRecordMark(view, start.id)).toBe(false);
  });
});
