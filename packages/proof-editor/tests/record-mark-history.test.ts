import { expect, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import { comment, suggestReplace } from "proof-sdk-upstream/src/editor/plugins/marks";
import { recordMarkHistoryPlugin } from "../src/record-mark-history";
import { removeRecordMark, retypeMark } from "../src/record-mark-retype";
import { withMarksEditor } from "./marks-editor";

// Every case runs on the editor `createProofEditor` builds, where prosemirror-history and
// y-prosemirror's UndoManager both listen, and presses the chords as a reader does.

const BY = "alice";
const SENTENCE = "The quick brown fox";
// "quick brown" inside the paragraph "The quick brown fox" (the paragraph opens at 0, text at 1).
const RANGE = { from: 5, to: 16 };
const BROWN = { from: 11, to: 16 };

function markTypes(view: EditorView): string[] {
  const types: string[] = [];
  view.state.doc.descendants((node) => {
    if (!node.isText) return true;
    for (const mark of node.marks) types.push(mark.type.name);
    return true;
  });
  return types;
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

function markAttrs(view: EditorView, markId: string): Record<string, unknown> | undefined {
  let attrs: Record<string, unknown> | undefined;
  view.state.doc.descendants((node) => {
    attrs ??= node.marks.find((mark) => mark.attrs.id === markId)?.attrs;
    return attrs === undefined;
  });
  return attrs;
}

const NOTHING_RECORDED = { history: 0, yjs: 0 };

test("after a Comment → Ask switch neither undo manager holds a step, and undo, undo, redo write no record mark", async () => {
  await withMarksEditor(SENTENCE, ({ depths, press, view }) => {
    const start = comment(view, "quick brown", BY, "", RANGE);
    const ask = retypeMark(view, start.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(markTypes(view)).toEqual(["dispatchAsk"]);
    expect(depths()).toEqual(NOTHING_RECORDED);
    press("Mod-z");
    press("Mod-z");
    press("Mod-Shift-z");
    expect(markTypes(view)).toEqual(["dispatchAsk"]);
  });
});

test("after a cancelled comment neither undo manager holds a step, and undo and redo write no record mark", async () => {
  await withMarksEditor(SENTENCE, ({ depths, press, view }) => {
    const start = comment(view, "quick brown", BY, "", RANGE);
    expect(removeRecordMark(view, start.id)).toBe(true);
    expect(markTypes(view)).toEqual([]);
    expect(depths()).toEqual(NOTHING_RECORDED);
    press("Mod-z");
    press("Mod-Shift-z");
    expect(markTypes(view)).toEqual([]);
  });
});

test("beside a recorded suggestion the composer's writes still leave no undo step", async () => {
  await withMarksEditor(SENTENCE, ({ depths, handle, press, view }) => {
    // A recorded suggestion: the bar wrote its mark with no replacement, and the server projects
    // the replacement and its own time into the marks map, which the browser reads without
    // touching the document. Upstream's creators then restamp that mark's attributes on every
    // later mark write: a removal and an addition of the same mark that are no displacement.
    const suggestion = suggestReplace(view, "quick", "bob", "", { from: 5, to: 10 });
    if (suggestion === null) throw new Error("the suggestion was not written");
    handle.applyRemoteMarks(
      {
        [suggestion.id]: {
          by: "bob",
          content: "red",
          createdAt: "2026-01-01T00:00:00.000Z",
          kind: "replace",
          quote: "quick",
          status: "pending",
        },
      },
      { hydrateAnchors: false }
    );
    expect(markAttrs(view, suggestion.id)?.content).toBe("");

    const start = comment(view, "brown", BY, "", BROWN);
    expect(markAttrs(view, suggestion.id)?.content).toBe("red");
    const ask = retypeMark(view, start.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(depths()).toEqual(NOTHING_RECORDED);
    press("Mod-z");
    press("Mod-z");
    press("Mod-Shift-z");
    expect(markTypes(view)).toEqual(["proofSuggestion", "dispatchAsk"]);
  });
});

test("a bar action that cuts into the open composer's own mark, which the margin then removes, leaves no undo step", async () => {
  await withMarksEditor(SENTENCE, ({ depths, handle, press, view }) => {
    // The reader comments on "quick brown", then refines the selection to "brown" before closing
    // that composer: the second comment cuts into the first composer's mark, the margin removes
    // the rest of it and the second composer takes over, as `composeForMark` does.
    const first = comment(view, "quick brown", BY, "", RANGE);
    handle.setComposerMark(first.id);
    const second = comment(view, "brown", BY, "", BROWN);
    removeRecordMark(view, first.id);
    handle.setComposerMark(second.id);
    expect(spansOf(view, second.id)).toEqual([BROWN]);
    expect(depths()).toEqual(NOTHING_RECORDED);
    // Cancelled, as Escape does.
    removeRecordMark(view, second.id);
    handle.setComposerMark(null);
    press("Mod-z");
    press("Mod-z");
    press("Mod-Shift-z");
    expect(markTypes(view)).toEqual([]);
  });
});

test("a bar Comment that cuts into another record's mark keeps its undo step", async () => {
  await withMarksEditor(SENTENCE, ({ depths, press, view }) => {
    // Bob's recorded comment covers "quick brown"; Alice's bar Comment over "brown" cuts it out
    // of Bob's comment (LEGION-458). That creation removes a mark no composer of hers holds, so it
    // is the reader's edit: one undo restores Bob's span, as on main.
    const bob = comment(view, "quick brown", "bob", "", RANGE);
    const alice = comment(view, "brown", BY, "", BROWN);
    expect(spansOf(view, bob.id)).toEqual([{ from: 5, to: 11 }]);
    expect(depths().history).toBe(1);
    press("Mod-z");
    expect(spansOf(view, bob.id)).toEqual([RANGE]);
    expect(spansOf(view, alice.id)).toEqual([]);
  });
});

test("a record-mark removal removeRecordMark did not make keeps its undo step", async () => {
  await withMarksEditor(SENTENCE, ({ depths, press, view }) => {
    // Only removeRecordMark labels its removal as the composer's own, with a key and value no
    // other module holds. Any other removal of a record mark - here one carrying string metas
    // named like that key - is an edit, and one undo restores the span.
    const bob = comment(view, "quick brown", "bob", "", RANGE);
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
    expect(depths().history).toBe(1);
    press("Mod-z");
    expect(spansOf(view, bob.id)).toEqual([RANGE]);
  });
});

test("the reader's own edits stay undoable", async () => {
  await withMarksEditor(SENTENCE, ({ depths, press, view }) => {
    view.dispatch(view.state.tr.insertText("!", 20, 20));
    expect(view.state.doc.textContent).toBe("The quick brown fox!");
    expect(depths().history).toBe(1);
    press("Mod-z");
    expect(view.state.doc.textContent).toBe("The quick brown fox");
    press("Mod-Shift-z");
    expect(view.state.doc.textContent).toBe("The quick brown fox!");
  });
});

// The control for the rows above: without the plugin, prosemirror-history records every mark
// write, and undo, undo, redo leaves the comment mark in the document again - the defect the
// plugin exists for.
test("without the plugin, undo, undo, redo puts the comment mark back", async () => {
  await withMarksEditor(SENTENCE, ({ depths, press, view }) => {
    const plugin = recordMarkHistoryPlugin.plugin();
    view.updateState(
      view.state.reconfigure({
        plugins: view.state.plugins.filter((candidate) => candidate !== plugin),
      })
    );
    const start = comment(view, "quick brown", BY, "", RANGE);
    const ask = retypeMark(view, start.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(depths().history).toBe(3);
    press("Mod-z");
    press("Mod-z");
    press("Mod-Shift-z");
    expect(markTypes(view)).toContain("proofComment");
  });
});
