import type { ResolvedPos } from "@milkdown/kit/prose/model";
import { Slice } from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import { CellSelection } from "@milkdown/kit/prose/tables";
import type { EditorView } from "@milkdown/kit/prose/view";
import * as proseView from "@milkdown/kit/prose/view";

/** prosemirror-view's own clipboard parser. It is marked `@internal`, so the type re-export
 *  `@milkdown/kit/prose/view` builds omits it, while the runtime export is there
 *  (prosemirror-view 1.42.3, `dist/index.js:5405` and its export list at `:5987`). The cast names
 *  the signature `doPaste` calls it with at `dist/index.js:3755`. */
const parseFromClipboard = (
  proseView as unknown as {
    __parseFromClipboard: (
      view: EditorView,
      text: string,
      html: string | null,
      plainText: boolean,
      context: ResolvedPos
    ) => Slice | null;
  }
).__parseFromClipboard;

/** A clipboard holding both of the types a browser copy writes. Either may be empty: a paste from
 *  a plain-text editor carries no `text/html`, and one from a spreadsheet carries both. */
export interface Clipboard {
  html?: string;
  text?: string;
}

/** The clipboard event a paste delivers. A clipboard without `text/html` is what a plain-text
 *  paste carries, so the markdown is parsed. */
function clipboardEvent({ html = "", text = "" }: Clipboard): ClipboardEvent {
  return {
    clipboardData: {
      getData: (type: string) => (type === "text/html" ? html : type === "text/plain" ? text : ""),
    },
  } as unknown as ClipboardEvent;
}

/**
 * What a browser paste reaches: `view.someProp("handlePaste", ...)` is what prosemirror-view's
 * `doPaste` calls, and happy-dom does not deliver a synthetic paste event to ProseMirror's own
 * listener, so this calls it directly, exercising the real clipboard plugin's `handlePaste`
 * unmodified (see clipboard-paste-recovery.test.ts). A paste whose DOM round trip reaches an
 * `instanceof HTMLElement` check needs `withHTMLElementGlobal` around it.
 */
export function pastePlainText(view: EditorView, text: string): void {
  view.someProp("handlePaste", (handlePaste) =>
    handlePaste(view, clipboardEvent({ text }), undefined as never)
  );
}

/**
 * A whole browser paste, with its slice: `doPaste` parses the clipboard into a slice before it
 * offers it to `handlePaste` (prosemirror-view 1.42.3, `doPaste`:
 * `parseFromClipboard(view, text, html, preferPlain, view.state.selection.$from)`, then
 * `handlePaste(view, event, slice || Slice.empty)`), and this builds the same slice through the
 * module's own exported `__parseFromClipboard`.
 *
 * `pastePlainText` passes no slice at all, which is enough for a paste the clipboard plugin
 * parses out of `text/plain` and nothing else. Every other case needs the slice: the patched
 * handler reads it for the cells a copy of a table carries (`htmlLineCells(slice, schema)`,
 * `__pastedCells(slice)` in `patches/@milkdown%2Fplugin-clipboard@7.22.1.patch`), and a plain-text
 * paste onto selected cells reaches `__pastedCells(slice)` too.
 */
export function pasteClipboard(view: EditorView, clipboard: Clipboard): void {
  const text = clipboard.text ?? "";
  const html = clipboard.html ?? "";
  const event = clipboardEvent(clipboard);
  const slice =
    parseFromClipboard(view, text, html === "" ? null : html, false, view.state.selection.$from) ??
    Slice.empty;
  view.someProp("handlePaste", (handlePaste) => handlePaste(view, event, slice));
}

/** `@milkdown/preset-gfm`'s `table_header_row.parseDOM` and `@milkdown/preset-commonmark`'s
 *  heading `parseDOM` check `dom instanceof HTMLElement` at runtime (a plain paste's DOM
 *  round-trip parses a table's or a heading's own serialized DOM back into a slice), and
 *  prosemirror-view's `readHTML` walks `document.styleSheets` for `CSSStyleRule`s when it reads a
 *  spreadsheet's clipboard HTML - while `withDomWindow` exposes only `document` and `window` from
 *  happy-dom's window onto `globalThis`. So such a paste throws `HTMLElement is not defined`, or
 *  `CSSStyleRule is not defined`, without the name it needs installed here. Scoped to the one call
 *  that needs it, restored after, the same save/restore shape `withDomWindow` itself uses. */
export function withDomGlobals<T>(names: readonly string[], run: () => T): T {
  const scope = globalThis as unknown as Record<string, unknown> & {
    window: Record<string, unknown>;
  };
  const previous = names.map((name) => [name, name in scope, scope[name]] as const);
  for (const name of names) scope[name] = scope.window[name];
  try {
    return run();
  } finally {
    for (const [name, had, value] of previous) {
      if (had) scope[name] = value;
      else delete scope[name];
    }
  }
}

/** The one name most pastes need. Kept as its own function because every caller before the
 *  clipboard matrix asks for exactly this. */
export function withHTMLElementGlobal<T>(run: () => T): T {
  return withDomGlobals(["HTMLElement"], run);
}

/** Puts the caret at document position `pos`, which is what a reader's click does: a headless run
 *  has no mouse, and the clipboard plugin reads the selection to decide what a paste means. */
export function placeCaret(view: EditorView, pos: number): void {
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, pos)));
}

/** Selects the cells of the rectangle whose corners are the cells at `anchorCellPos` and
 *  `headCellPos`, as dragging across a table does. Both positions are the cell nodes' own
 *  positions, which `cellPositions` below answers. */
export function selectCells(view: EditorView, anchorCellPos: number, headCellPos: number): void {
  view.dispatch(
    view.state.tr.setSelection(
      CellSelection.create(view.state.doc, anchorCellPos, headCellPos) as unknown as never
    )
  );
}

/** Every cell of the first table of the document, in document order, as `{ pos, text }`: the
 *  position a `CellSelection` takes and the position its text starts at, so a test names a cell
 *  by what is in it rather than by a number it computed itself. */
export function cellPositions(view: EditorView): Array<{ pos: number; text: string }> {
  const cells: Array<{ pos: number; text: string }> = [];
  view.state.doc.descendants((node, pos) => {
    if (node.type.name === "table_cell" || node.type.name === "table_header") {
      cells.push({ pos, text: node.textContent });
      return false;
    }
    return true;
  });
  return cells;
}
