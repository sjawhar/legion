import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { MarkAction } from "../src/lib";
import { withMarksEditor } from "./marks-editor";

const SENTENCE = "The quick brown fox";
// "quick brown" inside the paragraph "The quick brown fox" (the paragraph opens at 0, text at 1).
const RANGE = { from: 5, to: 16 };

function select(view: EditorView, from: number, to: number): void {
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
}

function commentSpans(view: EditorView) {
  const spans: { id: unknown; from: number; to: number }[] = [];
  view.state.doc.descendants((node, pos) => {
    for (const mark of node.marks) {
      if (mark.type.name === "proofComment") {
        spans.push({ from: pos, id: mark.attrs.id, to: pos + node.nodeSize });
      }
    }
    return true;
  });
  return spans;
}

test("commentOnSelection marks the selection as a comment and hands it to onMarkAction", async () => {
  const actions: MarkAction[] = [];
  await withMarksEditor(
    SENTENCE,
    ({ handle, view }) => {
      select(view, RANGE.from, RANGE.to);
      expect(handle.commentOnSelection()).toBe(true);
      expect(actions).toEqual([
        { from: 5, kind: "comment", markId: expect.any(String), quote: "quick brown", to: 16 },
      ]);
      expect(commentSpans(view)).toEqual([{ from: 5, id: actions[0]?.markId, to: 16 }]);
    },
    { onMarkAction: (action) => void actions.push(action) }
  );
});

test("commentOnSelection does nothing with no selection, nor while the document is read-only", async () => {
  const actions: MarkAction[] = [];
  await withMarksEditor(
    SENTENCE,
    ({ handle, view }) => {
      select(view, 5, 5);
      expect(handle.commentOnSelection()).toBe(false);

      select(view, RANGE.from, RANGE.to);
      handle.setReadOnly(true);
      expect(handle.commentOnSelection()).toBe(false);
      expect(actions).toEqual([]);
      expect(commentSpans(view)).toEqual([]);

      handle.setReadOnly(false);
      expect(handle.commentOnSelection()).toBe(true);
      expect(actions).toMatchObject([{ kind: "comment", quote: "quick brown" }]);
    },
    { onMarkAction: (action) => void actions.push(action) }
  );
  await withMarksEditor(
    SENTENCE,
    ({ handle, view }) => {
      select(view, RANGE.from, RANGE.to);
      expect(handle.commentOnSelection()).toBe(false);
      expect(commentSpans(view)).toEqual([]);
    },
    { onMarkAction: (action) => void actions.push(action), readOnly: true }
  );
  expect(actions).toMatchObject([{ kind: "comment", quote: "quick brown" }]);
});

test("the selection bar's Comment button takes the same path: a mark, then onMarkAction", async () => {
  const actions: MarkAction[] = [];
  await withMarksEditor(
    SENTENCE,
    ({ handle, view }) => {
      const button = [
        ...(view.dom.parentElement?.querySelectorAll<HTMLElement>(".dispatch-action-bar button") ??
          []),
      ].find((candidate) => candidate.textContent === "Comment");
      if (button === undefined) throw new Error("the selection bar has no Comment button");
      select(view, RANGE.from, RANGE.to);
      button.click();
      expect(actions).toEqual([
        { from: 5, kind: "comment", markId: expect.any(String), quote: "quick brown", to: 16 },
      ]);
      expect(commentSpans(view)).toEqual([{ from: 5, id: actions[0]?.markId, to: 16 }]);

      // Read-only, the bar is hidden, and a click that reaches it anyway marks nothing.
      select(view, 1, 4);
      handle.setReadOnly(true);
      button.click();
      expect(actions).toHaveLength(1);
    },
    { onMarkAction: (action) => void actions.push(action) }
  );
});
