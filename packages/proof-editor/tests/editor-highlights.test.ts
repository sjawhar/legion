import { expect, test } from "bun:test";
import type { EditorState } from "@milkdown/kit/prose/state";
import { DecorationSet } from "@milkdown/kit/prose/view";
import { comment } from "proof-sdk-upstream/src/editor/plugins/marks";
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
