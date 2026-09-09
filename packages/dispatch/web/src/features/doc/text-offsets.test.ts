import { expect, test } from "bun:test";
import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";

import { selectionToAnchor } from "./text-offsets";

test("selection anchors use CodeMirror's UTF-16 positions without conversion", () => {
  const parent = document.createElement("div");
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: "A😀Z",
      selection: { anchor: 1, head: 3 },
    }),
  });

  try {
    expect(selectionToAnchor(view)).toEqual({ from: 1, to: 3 });
  } finally {
    view.destroy();
  }
});
