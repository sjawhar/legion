import { expect, spyOn, test } from "bun:test";
import { Slice } from "@milkdown/kit/prose/model";
import type { EditorState } from "@milkdown/kit/prose/state";
import { DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { comment } from "proof-sdk-upstream/src/editor/plugins/marks";
import * as Y from "yjs";
import { blockIdOf } from "../src/editor/schema/block-ids";
import { editorHighlightsPlugin } from "../src/editor-highlights";
import { markedText } from "./mark-text";
import { withMarksEditor } from "./marks-editor";

function highlightDecorations(state: EditorState): DecorationSet {
  const plugin = editorHighlightsPlugin.plugin();
  const decorations = plugin.props.decorations;
  if (decorations === undefined) throw new Error("editor highlights plugin exposes no decorations");
  const source = decorations.call(plugin, state);
  if (!(source instanceof DecorationSet))
    throw new Error("editor highlights returned no DecorationSet");
  return source;
}

/** The text of each element the editor draws with `className`, in document order. */
function drawn(view: EditorView, className: string): string[] {
  return Array.from(
    view.dom.querySelectorAll(`.${className}`),
    (element) => element.textContent ?? ""
  );
}

/** A collaborator types `text` at the end of the document's block at `index`: another Y.Doc that
 *  holds `ydoc`'s state makes the edit, and its update reaches `ydoc` as the network delivers one. */
function typeAsCollaborator(ydoc: Y.Doc, index: number, text: string): void {
  const collaborator = new Y.Doc();
  Y.applyUpdate(collaborator, Y.encodeStateAsUpdate(ydoc));
  const block = collaborator.getXmlFragment("prosemirror").get(index);
  if (!(block instanceof Y.XmlElement)) throw new Error(`block ${index} is not an element`);
  const content = block.get(0);
  if (!(content instanceof Y.XmlText)) throw new Error(`block ${index} holds no text`);
  content.insert(content.length, text);
  Y.applyUpdate(ydoc, Y.encodeStateAsUpdate(collaborator, Y.encodeStateVector(ydoc)));
}

/** Runs `focus` with `window.setTimeout` held and returns the one timer it set, the pulse's end,
 *  unarmed, so the test decides when the pulse's duration has passed. */
function heldPulseEnd(focus: () => void): () => void {
  const timers: Array<() => void> = [];
  const setTimeout = spyOn(window, "setTimeout").mockImplementation(((callback: () => void) => {
    timers.push(callback);
    return 0;
  }) as typeof window.setTimeout);
  try {
    focus();
  } finally {
    setTimeout.mockRestore();
  }
  const [pulseEnd, ...others] = timers;
  if (pulseEnd === undefined || others.length > 0)
    throw new Error(`a focus set ${timers.length} timers, not one pulse end`);
  return pulseEnd;
}

test("active mark decorations are cached across selection-only transactions and map through typing", async () => {
  await withMarksEditor("The quick brown fox", ({ handle, view }) => {
    const mark = comment(view, "quick brown", "bob", "", { from: 5, to: 16 });
    handle.setActiveMarks([mark.id]);
    const first = highlightDecorations(view.state);
    expect(first.find()).toHaveLength(1);

    view.dispatch(view.state.tr.setSelection(view.state.selection));
    expect(highlightDecorations(view.state)).toBe(first);

    view.dispatch(view.state.tr.insertText("new ", 1));
    const mapped = highlightDecorations(view.state);
    expect(mapped.find()).toHaveLength(1);
    expect(markedText(view.state.doc, mark.id)).toBe("quick brown");
  });
});

test("a cached active mark decoration follows the mark when its span is removed", async () => {
  await withMarksEditor("The quick brown fox", ({ handle, view }) => {
    const mark = comment(view, "quick brown", "bob", "", { from: 5, to: 16 });
    handle.setActiveMarks([mark.id]);
    const recorded = view.state.doc
      .nodeAt(5)
      ?.marks.find((candidate) => candidate.attrs.id === mark.id);
    if (recorded === undefined) throw new Error("the comment mark is not in the document");

    view.dispatch(view.state.tr.removeMark(5, 16, recorded));

    expect(markedText(view.state.doc, mark.id)).toBe("");
    expect(highlightDecorations(view.state).find()).toHaveLength(0);
  });
});

test("text inserted with an active mark's id is drawn active too", async () => {
  await withMarksEditor("The quick brown fox", ({ handle, view }) => {
    const mark = comment(view, "quick brown", "bob", "", { from: 5, to: 16 });
    handle.setActiveMarks([mark.id]);
    const recorded = view.state.doc
      .nodeAt(5)
      ?.marks.find((candidate) => candidate.attrs.id === mark.id);
    if (recorded === undefined) throw new Error("the comment mark is not in the document");

    // "The quick brown fox" ends its paragraph at 20.
    view.dispatch(view.state.tr.insert(20, view.state.schema.text(" again", [recorded])));

    expect(markedText(view.state.doc, mark.id)).toBe("quick brown again");
    expect(drawn(view, "dispatch-mark-active")).toEqual(["quick brown", " again"]);
  });
});

test("a collaborator's edit after the active mark leaves it drawn", async () => {
  await withMarksEditor("The quick brown fox\n\nSecond paragraph", ({ handle, view, ydoc }) => {
    const mark = comment(view, "quick brown", "bob", "", { from: 5, to: 16 });
    handle.setActiveMarks([mark.id]);
    expect(drawn(view, "dispatch-mark-active")).toEqual(["quick brown"]);

    // y-prosemirror applies a remote update as one step that replaces the whole document, so the
    // step's content holds the active mark and, after it, text that carries none.
    typeAsCollaborator(ydoc, 1, "!");

    expect(view.state.doc.child(1).textContent).toBe("Second paragraph!");
    expect(drawn(view, "dispatch-mark-active")).toEqual(["quick brown"]);
  });
});

test("one insert of text with an active mark's id, then plain text, draws the marked text active", async () => {
  await withMarksEditor("The quick brown fox", ({ handle, view }) => {
    const mark = comment(view, "quick brown", "bob", "", { from: 5, to: 16 });
    handle.setActiveMarks([mark.id]);
    const recorded = view.state.doc
      .nodeAt(5)
      ?.marks.find((candidate) => candidate.attrs.id === mark.id);
    if (recorded === undefined) throw new Error("the comment mark is not in the document");
    const { schema } = view.state;

    // "The quick brown fox" ends its paragraph at 20.
    view.dispatch(
      view.state.tr.insert(20, [schema.text(" again", [recorded]), schema.text(" and plain")])
    );

    expect(markedText(view.state.doc, mark.id)).toBe("quick brown again");
    expect(drawn(view, "dispatch-mark-active")).toEqual(["quick brown", " again"]);
  });
});

test("an active or pulsed block is drawn on that block alone, through typing before it", async () => {
  await withMarksEditor("The quick brown fox\n\nSecond paragraph", ({ handle, view }) => {
    const second = blockIdOf(view.state.doc.child(1));
    if (second === null) throw new Error("the second paragraph has no block id");
    handle.setActiveBlocks([second]);
    expect(drawn(view, "dispatch-block-active")).toEqual(["Second paragraph"]);

    view.dispatch(view.state.tr.insertText("new ", 1));
    expect(drawn(view, "dispatch-block-active")).toEqual(["Second paragraph"]);

    const pulseEnd = heldPulseEnd(() => handle.focusBlock(second));
    expect(drawn(view, "dispatch-mark-pulse")).toEqual(["Second paragraph"]);
    pulseEnd();
    expect(drawn(view, "dispatch-mark-pulse")).toEqual([]);

    handle.setActiveBlocks([]);
    expect(drawn(view, "dispatch-block-active")).toEqual([]);
  });
});

test("an active block stays drawn through an edit that opens or closes it", async () => {
  // "First paragraph" is the document's first block: its text ends at 16, the second block's
  // starts at 18.
  const edits = [
    {
      name: "the next block joined into it",
      run: (view: EditorView) => view.state.tr.join(17),
      text: "First paragraphSecond paragraph",
    },
    {
      name: "a deletion across its end",
      run: (view: EditorView) => view.state.tr.delete(7, 25),
      text: "First paragraph",
    },
    {
      // Each pasted paragraph has an id of its own, so the paste stamps none. The replace puts a
      // copy of the first block, id and all, at the slice's open start.
      name: "two pasted paragraphs splitting it",
      run: (view: EditorView) => {
        const { schema } = view.state;
        const pasted = [" pasted", "Pasted"].map((text, index) =>
          schema.nodes.paragraph.create({ blockId: `pasted-${index}` }, schema.text(text))
        );
        const slice = new Slice(schema.nodes.doc.create(null, pasted).content, 1, 1);
        return view.state.tr.replace(16, 16, slice);
      },
      text: "First paragraph pasted",
    },
  ];
  const drawnAfter: Array<{ edit: string; drawn: string[] }> = [];
  for (const edit of edits) {
    await withMarksEditor("First paragraph\n\nSecond paragraph", ({ handle, view }) => {
      const first = blockIdOf(view.state.doc.child(0));
      if (first === null) throw new Error("the first paragraph has no block id");
      handle.setActiveBlocks([first]);
      expect(drawn(view, "dispatch-block-active")).toEqual(["First paragraph"]);

      view.dispatch(edit.run(view));

      expect({
        edit: edit.name,
        id: blockIdOf(view.state.doc.child(0)),
        text: view.state.doc.child(0).textContent,
      }).toEqual({ edit: edit.name, id: first, text: edit.text });
      drawnAfter.push({ edit: edit.name, drawn: drawn(view, "dispatch-block-active") });
    });
  }
  expect(drawnAfter).toEqual(edits.map((edit) => ({ edit: edit.name, drawn: [edit.text] })));
});

test("a pulsed mark stays pulsed until the latest focus's duration ends", async () => {
  await withMarksEditor("The quick brown fox", ({ handle, view }) => {
    const mark = comment(view, "quick brown", "bob", "", { from: 5, to: 16 });
    const firstEnd = heldPulseEnd(() => handle.focusMark(mark.id));
    expect(drawn(view, "dispatch-mark-pulse")).toEqual(["quick brown"]);

    // The reader focuses the mark again before the first pulse ends: the first pulse's timer
    // leaves the second pulse drawn.
    const secondEnd = heldPulseEnd(() => handle.focusMark(mark.id));
    firstEnd();
    expect(drawn(view, "dispatch-mark-pulse")).toEqual(["quick brown"]);

    secondEnd();
    expect(drawn(view, "dispatch-mark-pulse")).toEqual([]);
  });
});
