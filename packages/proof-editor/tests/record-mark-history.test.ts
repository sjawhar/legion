import { expect, test } from "bun:test";
import type { Ctx } from "@milkdown/kit/ctx";
import { history, redo, redoDepth, undo, undoDepth } from "@milkdown/kit/prose/history";
import type { Schema } from "@milkdown/kit/prose/model";
import { EditorState, type Plugin } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import { Window } from "happy-dom";
import { comment, marksPlugin } from "proof-sdk-upstream/src/editor/plugins/marks";
import {
  prosemirrorToYXmlFragment,
  redo as yRedo,
  ySyncPlugin,
  undo as yUndo,
  yUndoPlugin,
  yUndoPluginKey,
} from "y-prosemirror";
import * as Y from "yjs";
import { createHeadlessProof } from "../src/lib-headless.js";
import { recordMarkHistoryPlugin } from "../src/record-mark-history";
import { removeRecordMark, retypeMark } from "../src/record-mark-retype";

const BY = "alice";
// "quick brown" inside the paragraph "The quick brown fox" (the paragraph opens at 0, text at 1).
const RANGE = { from: 5, to: 16 };

function markTypes(view: EditorView): string[] {
  const types: string[] = [];
  view.state.doc.descendants((node) => {
    if (!node.isText) return true;
    for (const mark of node.marks) types.push(mark.type.name);
    return true;
  });
  return types;
}

interface UndoManagers {
  /** Undo and redo through the manager under test; each answers whether anything happened. */
  undo(view: EditorView): boolean;
  redo(view: EditorView): boolean;
  /** How many undo steps the manager holds. */
  depth(view: EditorView): number;
  /** Ends the current undo group, as the seconds between two readers' actions do: Yjs merges
   *  changes within its capture timeout into one item; prosemirror-history never groups mark
   *  steps, so it has nothing to end. */
  separate(view: EditorView): void;
}

function spansOf(view: EditorView, markId: string): { from: number; to: number }[] {
  const spans: { from: number; to: number }[] = [];
  view.state.doc.descendants((node, pos) => {
    if (!node.isText) return true;
    if (node.marks.some((mark) => mark.attrs.id === markId)) {
      const last = spans[spans.length - 1];
      if (last !== undefined && last.to === pos) last.to = pos + node.nodeSize;
      else spans.push({ from: pos, to: pos + node.nodeSize });
    }
    return true;
  });
  return spans;
}

/** A real EditorView over "The quick brown fox" with the upstream marks plugin, the history
 *  plugin under test, and whatever `plugins` adds. */
async function withEditor(
  plugins: (schema: Schema) => Plugin[],
  run: (view: EditorView) => void
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
    await recordMarkHistoryPlugin(proseContext)();
    const doc = schema.node("doc", null, [
      schema.node("paragraph", null, [schema.text("The quick brown fox")]),
    ]);
    const mount = window.document.body.appendChild(window.document.createElement("div"));
    const view = new EditorView(mount as unknown as HTMLElement, {
      state: EditorState.create({
        doc,
        plugins: [marksPlugin.plugin(), recordMarkHistoryPlugin.plugin(), ...plugins(schema)],
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

const proseMirrorHistory: UndoManagers = {
  depth: (view) => undoDepth(view.state),
  redo: (view) => redo(view.state, view.dispatch),
  separate: () => {},
  undo: (view) => undo(view.state, view.dispatch),
};

const yjsHistory: UndoManagers = {
  depth: (view) => yUndoPluginKey.getState(view.state)?.undoManager.undoStack.length ?? -1,
  redo: (view) => yRedo(view.state),
  separate: (view) => yUndoPluginKey.getState(view.state)?.undoManager.stopCapturing(),
  undo: (view) => yUndo(view.state),
};

/** y-prosemirror's sync and undo plugins over a Y.Doc seeded with the schema's document. */
function collabPlugins(schema: Schema): Plugin[] {
  const ydoc = new Y.Doc();
  const fragment = ydoc.getXmlFragment("prosemirror");
  prosemirrorToYXmlFragment(
    schema.node("doc", null, [
      schema.node("paragraph", null, [schema.text("The quick brown fox")]),
    ]),
    fragment
  );
  return [ySyncPlugin(fragment), yUndoPlugin()];
}

for (const [name, managers, plugins] of [
  ["prosemirror-history", proseMirrorHistory, () => [history()]],
  ["y-prosemirror's UndoManager", yjsHistory, collabPlugins],
] as const) {
  test(`${name}: neither undo nor redo writes a record mark after a Comment → Ask switch`, async () => {
    await withEditor(plugins, (view) => {
      const start = comment(view, "quick brown", BY, "", RANGE);
      const ask = retypeMark(view, start.id, "ask", BY);
      if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
      expect(markTypes(view)).toEqual(["dispatchAsk"]);
      // Nothing to undo: the bar's mark, the retype's mark and the removal were never recorded.
      expect(managers.depth(view)).toBe(0);
      expect(managers.undo(view)).toBe(false);
      expect(managers.undo(view)).toBe(false);
      expect(managers.redo(view)).toBe(false);
      expect(markTypes(view)).toEqual(["dispatchAsk"]);
    });
  });

  test(`${name}: neither undo nor redo writes a record mark after a cancelled comment`, async () => {
    await withEditor(plugins, (view) => {
      const start = comment(view, "quick brown", BY, "", RANGE);
      expect(removeRecordMark(view, start.id)).toBe(true);
      expect(markTypes(view)).toEqual([]);
      expect(managers.depth(view)).toBe(0);
      expect(managers.undo(view)).toBe(false);
      expect(managers.redo(view)).toBe(false);
      expect(markTypes(view)).toEqual([]);
    });
  });

  test(`${name}: a bar Comment that displaces another record's span keeps its undo step`, async () => {
    await withEditor(plugins, (view) => {
      // Bob's recorded comment covers "quick brown"; Alice's bar Comment over "brown" cuts it out
      // of Bob's comment (LEGION-458). That creation removes a mark it did not write, so it is
      // the reader's edit: one undo restores Bob's span, as on main.
      const bob = comment(view, "quick brown", "bob", "", RANGE);
      managers.separate(view);
      const alice = comment(view, "brown", BY, "", { from: 11, to: 16 });
      expect(spansOf(view, bob.id)).toEqual([{ from: 5, to: 11 }]);
      expect(spansOf(view, alice.id)).toEqual([{ from: 11, to: 16 }]);
      expect(managers.depth(view)).toBe(1);
      expect(managers.undo(view)).toBe(true);
      expect(spansOf(view, bob.id)).toEqual([{ from: 5, to: 16 }]);
      expect(spansOf(view, alice.id)).toEqual([]);
    });
  });

  test(`${name}: a record-mark removal removeRecordMark did not make keeps its undo step`, async () => {
    await withEditor(plugins, (view) => {
      // Only removeRecordMark marks its removal as the composer's own. Any other removal of a
      // record mark - here one carrying a string meta named like removeRecordMark's key - is an
      // edit, and one undo restores the span.
      const bob = comment(view, "quick brown", "bob", "", RANGE);
      managers.separate(view);
      const mark = view.state.doc
        .nodeAt(RANGE.from)
        ?.marks.find((candidate) => candidate.attrs.id === bob.id);
      if (mark === undefined) throw new Error("Bob's comment mark is not in the document");
      view.dispatch(
        view.state.tr
          .setMeta("recordMarkRemoval", true)
          .setMeta("recordMarkRemoval$", true)
          .removeMark(RANGE.from, RANGE.to, mark)
      );
      expect(spansOf(view, bob.id)).toEqual([]);
      expect(managers.depth(view)).toBe(1);
      expect(managers.undo(view)).toBe(true);
      expect(spansOf(view, bob.id)).toEqual([{ from: 5, to: 16 }]);
    });
  });

  test(`${name}: the reader's own edits stay undoable`, async () => {
    await withEditor(plugins, (view) => {
      view.dispatch(view.state.tr.insertText("!", 20, 20));
      expect(view.state.doc.textContent).toBe("The quick brown fox!");
      expect(managers.depth(view)).toBe(1);
      expect(managers.undo(view)).toBe(true);
      expect(view.state.doc.textContent).toBe("The quick brown fox");
      expect(managers.redo(view)).toBe(true);
      expect(view.state.doc.textContent).toBe("The quick brown fox!");
    });
  });
}

// The control for the rows above: without the plugin, prosemirror-history records every mark
// write, and undo-undo-redo leaves the comment mark in the document again - the defect the plugin
// exists for.
test("without the plugin, undo-undo-redo puts the comment mark back", async () => {
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
    const doc = schema.node("doc", null, [
      schema.node("paragraph", null, [schema.text("The quick brown fox")]),
    ]);
    const mount = window.document.body.appendChild(window.document.createElement("div"));
    const view = new EditorView(mount as unknown as HTMLElement, {
      state: EditorState.create({ doc, plugins: [marksPlugin.plugin(), history()], schema }),
    });
    try {
      const start = comment(view, "quick brown", BY, "", RANGE);
      const ask = retypeMark(view, start.id, "ask", BY);
      if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
      expect(undoDepth(view.state)).toBe(3);
      undo(view.state, view.dispatch);
      undo(view.state, view.dispatch);
      expect(redoDepth(view.state)).toBe(2);
      redo(view.state, view.dispatch);
      expect(markTypes(view)).toContain("proofComment");
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
});
