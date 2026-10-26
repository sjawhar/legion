import { expect, spyOn, test } from "bun:test";
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
      handle.setAccess("read");
      expect(handle.commentOnSelection()).toBe(false);
      expect(actions).toEqual([]);
      expect(commentSpans(view)).toEqual([]);

      handle.setAccess("edit");
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
    { access: "read", onMarkAction: (action) => void actions.push(action) }
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
      handle.setAccess("read");
      expect(bar.style.display).toBe("none");
      comment.click();
      expect(actions).toHaveLength(1);
    },
    { onMarkAction: (action) => void actions.push(action) }
  );
});

test("a document with comment access offers Comment alone, which reports the range and writes no mark", async () => {
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
        { from: 5, kind: "quote-comment", to: 16 },
        { from: 5, kind: "quote-comment", to: 16 },
      ]);
      // A Suggest or Ask click that reaches the hidden buttons does nothing.
      suggest.click();
      ask.click();
      expect(actions).toHaveLength(2);
      expect(commentSpans(view)).toEqual([]);

      // Read access hides the bar; edit access brings every action back.
      handle.setAccess("read");
      expect(bar.style.display).toBe("none");
      expect(handle.commentOnSelection()).toBe(false);
      handle.setAccess("edit");
      expect(bar.style.display).toBe("flex");
      expect([comment.hidden, suggest.hidden, ask.hidden]).toEqual([false, false, false]);
      expect(handle.commentOnSelection()).toBe(true);
      expect(actions.at(-1)).toMatchObject({ kind: "comment", markId: expect.any(String) });
      expect(commentSpans(view)).toHaveLength(1);
    },
    { access: "comment", onMarkAction: (action) => void actions.push(action) }
  );
});

// A comment on a document with comment access names its text by quote, so a selection with no
// text to quote - an image alone, or only a space - is offered nothing, and the host never hears
// of it.
test("a document with comment access offers nothing for a selection that holds no text", async () => {
  const actions: MarkAction[] = [];
  await withMarksEditor(
    `${SENTENCE}\n\n![a diagram](diagram.png)`,
    ({ handle, view }) => {
      const { bar, comment } = selectionBar(view);
      let image: number | undefined;
      view.state.doc.descendants((node, pos) => {
        if (node.type.name === "image") image = pos;
      });
      if (image === undefined) throw new Error("the document has no image");

      for (const [from, to] of [
        [image, image + 1],
        [4, 5],
      ] as const) {
        select(view, from, to);
        expect(bar.style.display).toBe("none");
        comment.click();
        expect(handle.commentOnSelection()).toBe(false);
      }
      expect(actions).toEqual([]);

      // Text beside the image is still offered.
      select(view, RANGE.from, RANGE.to);
      expect(bar.style.display).toBe("flex");
      expect(handle.commentOnSelection()).toBe(true);
      expect(actions).toEqual([{ from: 5, kind: "quote-comment", to: 16 }]);
    },
    { access: "comment", onMarkAction: (action) => void actions.push(action) }
  );
});

test("a rejected quote-comment leaves the document as it was and reports the rejection", async () => {
  const actions: MarkAction[] = [];
  const rejection = new Error("composer closed");
  const warn = spyOn(console, "warn").mockImplementation(() => {});
  try {
    await withMarksEditor(
      SENTENCE,
      async ({ handle, view }) => {
        const before = view.state.doc;
        select(view, RANGE.from, RANGE.to);
        expect(handle.commentOnSelection()).toBe(true);
        await Promise.resolve();
        await Promise.resolve();
        expect(actions).toMatchObject([{ kind: "quote-comment" }]);
        expect(view.state.doc.eq(before)).toBe(true);
        expect(warn).toHaveBeenCalledWith(
          "[@sjawhar/proof-editor] onMarkAction(quote-comment) rejected",
          rejection
        );
      },
      {
        access: "comment",
        onMarkAction: (action) => {
          actions.push(action);
          return Promise.reject(rejection);
        },
      }
    );
  } finally {
    warn.mockRestore();
  }
});
