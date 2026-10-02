import { expect, spyOn, test } from "bun:test";
import type { EditorState } from "@milkdown/kit/prose/state";
import { DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { comment } from "proof-sdk-upstream/src/editor/plugins/marks";
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
