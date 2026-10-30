import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { MarkAction } from "../src/lib";
import { blockIdOf } from "../src/lib";
import { QUOTE_CONTEXT_CHARS } from "../src/record-mark-retype";
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
        {
          from: 5,
          kind: "comment",
          markId: expect.any(String),
          placement: { blockId: expect.any(String), prefix: "The ", suffix: " fox" },
          quote: "quick brown",
          to: 16,
        },
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
        {
          from: 5,
          kind: "comment",
          markId: expect.any(String),
          placement: { blockId: expect.any(String), prefix: "The ", suffix: " fox" },
          quote: "quick brown",
          to: 16,
        },
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

test("the selection bar records the words' surroundings, capped, and shorter at a block's edge", async () => {
  const actions: MarkAction[] = [];
  const quote = "the lazy dog";
  // More text than the cap on each side of the selected words, so both sides are cut down to it.
  const before = "A sentence written out at length, so the words before run past the cap. ";
  const after = " And a sentence after them, also written out well past the cap it keeps.";
  // The paragraph opens at 0 and its text at 1, so a text offset is one less than the position.
  const at = before.length + 1;
  await withMarksEditor(
    `${before}${quote}${after}`,
    ({ handle, view }) => {
      const blockId = blockIdOf(view.state.doc.child(0));
      if (blockId === null) throw new Error("the paragraph has no block id");
      select(view, at, at + quote.length);
      expect(handle.commentOnSelection()).toBe(true);
      // The quote alone cannot say which reading of those words the reader selected, and the host
      // keeps this for the day the mark is gone and the editor remembers nothing
      // (`remarkRecordMark`): the text around them, as the editor read it, capped on each side.
      expect(actions).toEqual([
        {
          from: at,
          kind: "comment",
          markId: expect.any(String),
          placement: {
            blockId,
            prefix: before.slice(-QUOTE_CONTEXT_CHARS),
            suffix: after.slice(0, QUOTE_CONTEXT_CHARS),
          },
          quote,
          to: at + quote.length,
        },
      ]);
      const written = actions[0];
      if (written === undefined || written.kind !== "comment") {
        throw new Error("the bar reported no comment action");
      }
      expect(written.placement?.prefix).toHaveLength(QUOTE_CONTEXT_CHARS);
      expect(written.placement?.suffix).toHaveLength(QUOTE_CONTEXT_CHARS);
    },
    { onMarkAction: (action) => void actions.push(action) }
  );

  // A selection against an edge of its block keeps what text there is beside it, which at the
  // edge itself is none at all.
  const edges: MarkAction[] = [];
  const edged = `${quote} sits at the start, and at the end sits ${quote}`;
  await withMarksEditor(
    edged,
    ({ handle, view }) => {
      select(view, 1, 1 + quote.length);
      expect(handle.commentOnSelection()).toBe(true);
      const end = edged.length + 1;
      select(view, end - quote.length, end);
      expect(handle.commentOnSelection()).toBe(true);
      expect(edges.map((action) => (action.kind === "comment" ? action.placement : null))).toEqual([
        {
          blockId: expect.any(String),
          prefix: "",
          suffix: edged.slice(quote.length, quote.length + QUOTE_CONTEXT_CHARS),
        },
        {
          blockId: expect.any(String),
          prefix: edged.slice(0, edged.length - quote.length).slice(-QUOTE_CONTEXT_CHARS),
          suffix: "",
        },
      ]);
    },
    { onMarkAction: (action) => void edges.push(action) }
  );
});
