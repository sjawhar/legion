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

test("a refused paste leaves the next paste's text intact instead of poisoning every later one", async () => {
  await withMarksEditor("", async ({ view }) => {
    // A mark span of a kind this schema has no parser for (only `dispatch: "ask"` matches):
    // throws inside the clipboard plugin's own `parsePlainText`, uncaught.
    expect(() => pastePlainText(view, 'see <span data-dispatch="bogus">this</span> now')).toThrow();

    // Before the fix: the single `parserCtx` ParserState stays corrupted, so perfectly ordinary
    // text pasted next throws too (and never reaches the document) for the rest of the session.
    expect(() => pastePlainText(view, "Fixed two bugs")).not.toThrow();
    expect(view.state.doc.textContent).toContain("Fixed two bugs");

    // The engine recovers for good, not just once: a second ordinary paste still works.
    expect(() => pastePlainText(view, "and another")).not.toThrow();
    expect(view.state.doc.textContent).toContain("and another");
  });
});
