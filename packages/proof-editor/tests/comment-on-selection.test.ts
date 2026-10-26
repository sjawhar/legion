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

/** The mark an action names; a `quote-comment` names none. */
function markIdOf(action: MarkAction | undefined): string | undefined {
  return action !== undefined && "markId" in action ? action.markId : undefined;
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
      expect(commentSpans(view)).toEqual([{ from: 5, id: markIdOf(actions[0]), to: 16 }]);
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

/** The selection bar's buttons by label, and the bar itself. */
function selectionBar(view: EditorView) {
  const bar = view.dom.parentElement?.querySelector<HTMLElement>(".dispatch-action-bar");
  if (bar === null || bar === undefined) throw new Error("the editor has no selection bar");
  const button = (label: string) => {
    const found = [...bar.querySelectorAll<HTMLButtonElement>("button")].find(
      (candidate) => candidate.textContent === label
    );
    if (found === undefined) throw new Error(`the selection bar has no ${label} button`);
    return found;
  };
  return { ask: button("Ask"), bar, comment: button("Comment"), suggest: button("Suggest") };
}

test("the selection bar's Comment button takes the same path: a mark, then onMarkAction", async () => {
  const actions: MarkAction[] = [];
  await withMarksEditor(
    SENTENCE,
    ({ handle, view }) => {
      const { bar, comment } = selectionBar(view);
      select(view, RANGE.from, RANGE.to);
      comment.click();
      expect(actions).toEqual([
        { from: 5, kind: "comment", markId: expect.any(String), quote: "quick brown", to: 16 },
      ]);
      expect(commentSpans(view)).toEqual([{ from: 5, id: markIdOf(actions[0]), to: 16 }]);

      // Read-only, the bar hides itself, and a click that reaches it anyway marks nothing.
      select(view, 1, 4);
      handle.setReadOnly(true);
      expect(bar.style.display).toBe("none");
      comment.click();
      expect(actions).toHaveLength(1);
    },
    { onMarkAction: (action) => void actions.push(action) }
  );
});

test("a read-only document that takes comments offers Comment alone, which reports the quote and writes no mark", async () => {
  const actions: MarkAction[] = [];
  await withMarksEditor(
    SENTENCE,
    ({ handle, view }) => {
      const { ask, bar, comment, suggest } = selectionBar(view);
      select(view, RANGE.from, RANGE.to);
      expect(bar.style.display).toBe("flex");
      expect([comment.hidden, suggest.hidden, ask.hidden]).toEqual([false, true, true]);

      comment.click();
      expect(handle.commentOnSelection()).toBe(true);
      expect(actions).toEqual([
        { from: 5, kind: "quote-comment", quote: "quick brown", to: 16 },
        { from: 5, kind: "quote-comment", quote: "quick brown", to: 16 },
      ]);
      // A Suggest or Ask click that reaches the hidden buttons does nothing.
      suggest.click();
      ask.click();
      expect(actions).toHaveLength(2);
      expect(commentSpans(view)).toEqual([]);

      // Taking comments off hides the bar; making the document editable brings every action back.
      handle.setReadOnly(true, false);
      expect(bar.style.display).toBe("none");
      expect(handle.commentOnSelection()).toBe(false);
      handle.setReadOnly(false);
      expect(bar.style.display).toBe("flex");
      expect([comment.hidden, suggest.hidden, ask.hidden]).toEqual([false, false, false]);
      expect(handle.commentOnSelection()).toBe(true);
      expect(actions.at(-1)).toMatchObject({ kind: "comment", markId: expect.any(String) });
      expect(commentSpans(view)).toHaveLength(1);
    },
    {
      onMarkAction: (action) => void actions.push(action),
      readOnly: true,
      readOnlyComments: true,
    }
  );
});

test("a rejected quote-comment leaves the document as it was", async () => {
  const actions: MarkAction[] = [];
  await withMarksEditor(
    SENTENCE,
    async ({ handle, view }) => {
      const before = view.state.doc;
      select(view, RANGE.from, RANGE.to);
      expect(handle.commentOnSelection()).toBe(true);
      await Promise.resolve();
      expect(actions).toMatchObject([{ kind: "quote-comment" }]);
      expect(view.state.doc.eq(before)).toBe(true);
    },
    {
      onMarkAction: (action) => {
        actions.push(action);
        return Promise.reject(new Error("composer closed"));
      },
      readOnly: true,
      readOnlyComments: true,
    }
  );
});
