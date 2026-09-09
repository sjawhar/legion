import type { EditorView } from "@codemirror/view";

/** Returns the current CodeMirror selection as a Dispatch anchor range. */
export function selectionToAnchor(view: EditorView): { from: number; to: number } {
  const selection = view.state.selection.main;
  return { from: selection.from, to: selection.to };
}
