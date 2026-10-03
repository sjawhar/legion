import { expect, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { MarkRange } from "proof-sdk-upstream/src/editor/plugins/marks";
import { comment, suggestReplace } from "proof-sdk-upstream/src/editor/plugins/marks";
import { createAskMark } from "../src/dispatch-marks";
import { markIdAt } from "../src/record-mark-target";
import { withMarksEditor } from "./marks-editor";

const SENTENCE = "The quick brown fox";
// Inside the paragraph "The quick brown fox" (the paragraph opens at 0, its text at 1).
const WIDE = { from: 5, to: 16 }; // "quick brown"
const NARROW = { from: 11, to: 16 }; // "brown"
const IN_BROWN = 13;

type Kind = "comment" | "suggestion" | "ask";

/** Marks `range` with a record mark of `kind`, the way the selection bar does, and answers its id. */
function mark(view: EditorView, kind: Kind, range: MarkRange): string {
  const quote = view.state.doc.textBetween(range.from, range.to);
  if (kind === "comment") return comment(view, quote, "alice", "", range).id;
  const created =
    kind === "suggestion"
      ? suggestReplace(view, quote, "alice", "", range)
      : createAskMark(view, range, "alice");
  if (created === null) throw new Error(`the ${kind} over "${quote}" was not written`);
  return created.id;
}

/** The element a click on the character at `pos` lands on: the innermost span around its text. */
function clickTarget(view: EditorView, pos: number): HTMLElement {
  const { node } = view.domAtPos(pos);
  expect(node.nodeType).toBe(3);
  if (node.parentElement === null) throw new Error("the text has no element around it");
  return node.parentElement;
}

for (const kind of ["comment", "suggestion", "ask"] as const) {
  test(`a click on text a wider and a narrower ${kind} both cover names the narrower, whichever was made first`, async () => {
    const innermost: string[] = [];
    for (const first of ["wider", "narrower"] as const) {
      await withMarksEditor(SENTENCE, ({ view }) => {
        const ranges = first === "wider" ? [WIDE, NARROW] : [NARROW, WIDE];
        const [firstId, secondId] = ranges.map((range) => mark(view, kind, range));
        const [wide, narrow] = first === "wider" ? [firstId, secondId] : [secondId, firstId];
        const target = clickTarget(view, IN_BROWN);
        innermost.push(
          target.closest("[data-id]")?.getAttribute("data-id") === wide ? "wider" : "narrower"
        );

        expect(markIdAt(view, target)).toBe(narrow);
        // Text only the wider mark covers names it.
        expect(markIdAt(view, clickTarget(view, 7))).toBe(wide);
      });
    }
    // The order the marks are made in decides which span nests inside the other, so one of the two
    // puts the wider span innermost: the case a click on the innermost span got wrong.
    expect([...innermost].sort()).toEqual(["narrower", "wider"]);
  });
}

test("of two marks over the same text, a click names the innermost span's", async () => {
  await withMarksEditor(SENTENCE, ({ view }) => {
    const ids = [mark(view, "comment", NARROW), mark(view, "ask", NARROW)];
    const target = clickTarget(view, IN_BROWN);
    const inner = target.closest("[data-id]")?.getAttribute("data-id");
    expect(ids).toContain(inner ?? "");

    expect(markIdAt(view, target)).toBe(inner ?? "");
  });
});
