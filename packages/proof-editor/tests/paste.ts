import { Slice } from "@milkdown/kit/prose/model";
import type { EditorView } from "@milkdown/kit/prose/view";
import * as prosemirrorView from "@milkdown/kit/prose/view";

/** The clipboard event a plain-text paste delivers: no `text/html`, so the markdown is parsed. */
export function plainTextPasteEvent(text: string): ClipboardEvent {
  return {
    clipboardData: { getData: (type: string) => (type === "text/plain" ? text : "") },
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
    handlePaste(view, plainTextPasteEvent(text), undefined as never)
  );
}

/** prosemirror-view's own clipboard read, which it exports for tests but does not declare. */
const { __parseFromClipboard: parseFromClipboard } = prosemirrorView as unknown as {
  __parseFromClipboard(
    view: EditorView,
    text: string,
    html: string | null,
    plainText: boolean,
    $context: EditorView["state"]["selection"]["$from"]
  ): Slice | null;
};

/**
 * What a browser paste of `html` (with `text` as its plain-text flavour) does, as
 * prosemirror-view's `doPaste` does it: the view parses the HTML with the schema's DOM parser,
 * offers the slice to every `handlePaste` (the clipboard plugin's takes it), and inserts the slice
 * itself when none does. happy-dom does not deliver a synthetic paste event to ProseMirror's own
 * listener, so this makes the same calls.
 */
export function pasteHtml(view: EditorView, html: string, text = ""): void {
  const event = {
    clipboardData: {
      getData: (type: string) => (type === "text/html" ? html : type === "text/plain" ? text : ""),
    },
  } as unknown as ClipboardEvent;
  const slice = parseFromClipboard(view, text, html, false, view.state.selection.$from);
  if (
    view.someProp("handlePaste", (handlePaste) => handlePaste(view, event, slice ?? Slice.empty))
  ) {
    return;
  }
  if (slice) view.dispatch(view.state.tr.replaceSelection(slice));
}

/** `@milkdown/preset-gfm`'s `table_header_row.parseDOM` and `@milkdown/preset-commonmark`'s
 *  heading `parseDOM` check `dom instanceof HTMLElement` at runtime (a plain paste's DOM
 *  round-trip parses a table's or a heading's own serialized DOM back into a slice), and
 *  `withDomWindow` exposes only `document` and `window` from happy-dom's window onto
 *  `globalThis`, not its `HTMLElement` - so such a paste throws `HTMLElement is not defined`
 *  without it. Scoped to the one call that needs it, restored after, the same save/restore shape
 *  `withDomWindow` itself uses. */
export function withHTMLElementGlobal<T>(run: () => T): T {
  const scope = globalThis as { HTMLElement?: unknown; window: { HTMLElement: unknown } };
  const previous = scope.HTMLElement;
  scope.HTMLElement = scope.window.HTMLElement;
  try {
    return run();
  } finally {
    if (previous === undefined) delete scope.HTMLElement;
    else scope.HTMLElement = previous;
  }
}
