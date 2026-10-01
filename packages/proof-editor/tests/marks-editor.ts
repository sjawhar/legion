import { undoDepth } from "@milkdown/kit/prose/history";
import type { EditorView } from "@milkdown/kit/prose/view";
import { Window } from "happy-dom";
import { prosemirrorToYXmlFragment, yUndoPluginKey } from "y-prosemirror";
import * as Y from "yjs";
import { createProofEditor, type ProofEditorHandle } from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";

export interface MarksEditor {
  readonly handle: ProofEditorHandle;
  readonly view: EditorView;
  /** Presses Mod-z (undo) or Mod-Shift-z (redo) in the editor, as a reader does: the history
   *  keymap takes it first, and the collab keymap when that one declines. */
  press(chord: "Mod-z" | "Mod-Shift-z"): void;
  /** The undo steps each manager holds: prosemirror-history's, and y-prosemirror's UndoManager's. */
  depths(): { history: number; yjs: number };
}

/**
 * Runs `run` against the editor `createProofEditor` builds - Milkdown's presets, the collab
 * binding with both undo managers, and every plugin of the stack - in happy-dom, configured as
 * Dispatch's margin configures it, over a Y.Doc seeded with `markdown`. The real stack matters:
 * plugins such as commonmark's hard-break mark cleaner append transactions after a mark write, and
 * only the whole stack shows what reaches each undo manager.
 */
export async function withMarksEditor(
  markdown: string,
  run: (editor: MarksEditor) => void | Promise<void>
): Promise<void> {
  const { parseMarkdown } = await createHeadlessProof();
  const ydoc = new Y.Doc();
  prosemirrorToYXmlFragment(parseMarkdown(markdown), ydoc.getXmlFragment("prosemirror"));
  const window = new Window({ url: "http://localhost/" });
  // Put back exactly what was there: a key Bun never defined is deleted, not left as undefined,
  // because src/tests/headless-no-dom.test.ts asserts `!("document" in globalThis)`.
  const previous = (["document", "window"] as const).map(
    (key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)] as const
  );
  Object.assign(globalThis, { document: window.document, window });
  try {
    const root = window.document.body.appendChild(window.document.createElement("div"));
    const handle = await createProofEditor(root as unknown as HTMLElement, {
      heatMapMode: "hidden",
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc,
    });
    const { view } = handle;
    try {
      await run({
        depths: () => ({
          history: undoDepth(view.state),
          yjs: yUndoPluginKey.getState(view.state)?.undoManager.undoStack.length ?? -1,
        }),
        handle,
        press: (chord) => {
          const redo = chord === "Mod-Shift-z";
          // A browser reports Shift+Z as key "Z" with key code 90; prosemirror-keymap finds
          // "Mod-Shift-z" through the key code.
          const event = new window.KeyboardEvent("keydown", {
            bubbles: true,
            cancelable: true,
            ctrlKey: true,
            key: redo ? "Z" : "z",
            keyCode: 90,
            shiftKey: redo,
          }) as unknown as KeyboardEvent;
          // What ProseMirror's own keydown listener does with the event: offer it to every
          // plugin's handleKeyDown in order. happy-dom does not deliver a synthetic keydown to
          // that listener, so the harness makes the call itself.
          view.someProp("handleKeyDown", (handleKeyDown) => handleKeyDown(view, event));
        },
        view,
      });
    } finally {
      handle.destroy();
      // The editor tears its view down asynchronously; its plugins' teardown needs the DOM.
      while (!view.isDestroyed) await Promise.resolve();
    }
  } finally {
    for (const [key, descriptor] of previous) {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, key);
      else Object.defineProperty(globalThis, key, descriptor);
    }
    await window.happyDOM.close();
  }
}
