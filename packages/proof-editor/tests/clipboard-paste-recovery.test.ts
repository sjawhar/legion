import { expect, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import { withMarksEditor } from "./marks-editor";

// LEGION-566. Milkdown's own `parser` plugin builds `parserCtx`'s ParserState once, at boot, and
// never rebuilds it (@milkdown/core: `ctx.set(parserCtx, ParserState.create(schema, remark))`).
// `@milkdown/plugin-clipboard` reads that same singleton for every text/plain paste
// (`ctx.get(parserCtx)(text)`, uncaught), so a parse it refuses - a mark span of a kind this
// schema has no parser for, exactly markdown-engine.test.ts's LEGION-540 fixture - leaves its
// stack open and poisons every later paste in the same editor, for the rest of the session.
//
// `view.someProp("handlePaste", f => f(view, event, slice))` is what a real browser paste event
// reaches (prosemirror-view's `doPaste`); happy-dom does not deliver a synthetic paste event to
// ProseMirror's own listener (marks-editor.ts's `press` makes the same call for keydown), so the
// harness calls it directly, exercising the real clipboard plugin's `handlePaste` unmodified.

function plainTextPasteEvent(text: string): ClipboardEvent {
  return {
    clipboardData: { getData: (type: string) => (type === "text/plain" ? text : "") },
  } as unknown as ClipboardEvent;
}

function pastePlainText(view: EditorView, text: string): void {
  view.someProp("handlePaste", (handlePaste) =>
    handlePaste(view, plainTextPasteEvent(text), undefined as never)
  );
}

/** `@milkdown/preset-gfm`'s `table_header_row.parseDOM` checks `dom instanceof HTMLElement` at
 *  runtime (a plain paste's DOM round-trip parses a table's own serialized DOM back into a
 *  slice), and `withDomWindow` exposes only `document` and `window` from happy-dom's window onto
 *  `globalThis`, not its `HTMLElement` - so a table paste throws `HTMLElement is not defined`
 *  without it. Scoped to the one call that needs it, restored after, the same save/restore shape
 *  `withDomWindow` itself uses. */
function withHTMLElementGlobal<T>(run: () => T): T {
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

test("a refused paste leaves the next paste's text intact instead of poisoning every later one", async () => {
  await withMarksEditor("", async ({ view }) => {
    // A mark span of a kind this schema has no parser for (only `dispatch: "ask"` matches):
    // throws inside the clipboard plugin's own `parsePlainText`, uncaught.
    expect(() => pastePlainText(view, 'see <span data-dispatch="bogus">this</span> now')).toThrow();

    // The heal costs no editor feature: the fresh parser built after the throw still handles a
    // GFM table, a bold span inside a cell, and this library's own `dispatchAsk` span (the only
    // mark the schema has, needing no backend record to parse) exactly as the first, uncorrupted
    // parser would have.
    const table =
      '| a | b |\n| --- | --- |\n| **bold** | <span data-dispatch="ask" data-id="a1" data-by="human:alice">q</span> |\n';
    expect(() => withHTMLElementGlobal(() => pastePlainText(view, table))).not.toThrow();
    expect(view.state.doc.textContent).toContain("q");
    expect(view.state.doc.textContent).toContain("bold");
    const types: string[] = [];
    let sawTable = false;
    view.state.doc.descendants((node) => {
      if (node.type.name === "table") sawTable = true;
      for (const mark of node.marks) types.push(mark.type.name);
      return true;
    });
    expect(sawTable).toBe(true);
    expect(types).toContain("strong");
    expect(types).toContain("dispatchAsk");

    // The recovery is permanent, not one-shot: a further ordinary paste still works.
    expect(() => pastePlainText(view, "and another")).not.toThrow();
    expect(view.state.doc.textContent).toContain("and another");
  });
});
