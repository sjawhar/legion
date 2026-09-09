import { expect, test } from "bun:test";
import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";

import {
  codeMirrorPositionToYjs,
  lengthUtf16,
  selectionToAnchor,
  sliceUtf16,
  yjsPositionToCodeMirror,
} from "./text-offsets";

test("UTF-16 helpers preserve emoji range boundaries", () => {
  const markdown = "A😀é";

  expect(lengthUtf16(markdown)).toBe(4);
  expect(sliceUtf16(markdown, 1, 3)).toBe("😀");
});

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

test("Yjs and CodeMirror positions are the same UTF-16 offset", () => {
  expect(yjsPositionToCodeMirror(3)).toBe(3);
  expect(codeMirrorPositionToYjs(3)).toBe(3);
});
