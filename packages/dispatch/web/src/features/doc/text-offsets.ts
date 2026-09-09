import type { EditorView } from "@codemirror/view";

/**
 * Dispatch offsets are JavaScript UTF-16 code units. CodeMirror and Yjs use
 * the same unit, so positions flow between them without conversion.
 */
export function lengthUtf16(text: string): number {
  return text.length;
}

/** Slices text at Dispatch/Yjs/CodeMirror UTF-16 code-unit offsets. */
export function sliceUtf16(text: string, from: number, to?: number): string {
  return text.slice(from, to);
}

/**
 * Converts a Yjs index to a CodeMirror position. Both use JS UTF-16 units, so
 * conversion is deliberately an identity rather than a code-point conversion.
 */
export function yjsPositionToCodeMirror(position: number): number {
  return position;
}

/** Converts a CodeMirror position to a Yjs index (the same UTF-16 unit). */
export function codeMirrorPositionToYjs(position: number): number {
  return position;
}

/** Returns the current CodeMirror selection as a Dispatch anchor range. */
export function selectionToAnchor(view: EditorView): { from: number; to: number } {
  const selection = view.state.selection.main;
  return { from: selection.from, to: selection.to };
}
