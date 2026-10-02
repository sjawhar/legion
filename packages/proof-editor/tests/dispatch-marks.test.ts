/**
 * Two readers' asks may cover the same text: the `dispatchAsk` schema declares `excludes: ''`, so
 * a second ask over part of the first adds its own mark instead of cutting the first out of the
 * overlap, removing one ask removes that mark's instance, never every ask on its text, and an ask
 * span nested in another reads back from markdown as both.
 */

import { expect, test } from "bun:test";
import { EditorState, type Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { createAskMark, findAskMarkRange, removeAskMark } from "../src/dispatch-marks.js";
import { createHeadlessProof } from "../src/lib-headless.js";
import { markedText } from "./mark-text";

interface ViewDouble {
  view: EditorView;
  transactions: Transaction[];
}

/** The two members createAskMark and removeAskMark read: the state, and dispatch applying to it. */
function viewOver(state: EditorState): ViewDouble {
  const transactions: Transaction[] = [];
  const double = {
    state,
    dispatch(tr: Transaction) {
      transactions.push(tr);
      double.state = double.state.apply(tr);
    },
  };
  return { view: double as unknown as EditorView, transactions };
}

async function twoAsks() {
  const { schema } = await createHeadlessProof();
  const doc = schema.node("doc", null, [
    schema.node("paragraph", null, [schema.text("The quick brown fox")]),
  ]);
  const { view, transactions } = viewOver(EditorState.create({ doc, schema }));
  // "The quick brown fox": "quick brown" is 5..16, "brown" 11..16 (the paragraph opens at 0).
  const bob = createAskMark(view, { from: 5, to: 16 }, "user:bob");
  const alice = createAskMark(view, { from: 11, to: 16 }, "user:alice");
  if (bob === null || alice === null)
    throw new Error("createAskMark refused a plain paragraph range");
  return { view, transactions, bob: bob.id, alice: alice.id };
}

test("an ask over part of another ask leaves the first whole", async () => {
  const { view, transactions, bob, alice } = await twoAsks();
  const doc = view.state.doc;
  expect(markedText(doc, bob)).toBe("quick brown");
  expect(markedText(doc, alice)).toBe("brown");
  expect(findAskMarkRange(doc, bob)).toEqual({ from: 5, to: 16 });
  expect(findAskMarkRange(doc, alice)).toEqual({ from: 11, to: 16 });
  const second = transactions[1];
  if (second === undefined) throw new Error("the second createAskMark dispatched nothing");
  expect(second.steps.map((step) => step.toJSON().stepType)).toEqual(["addMark"]);
});

test("removing one of two overlapping asks leaves the other whole", async () => {
  for (const [removed, kept, keptText] of [
    ["alice", "bob", "quick brown"],
    ["bob", "alice", "brown"],
  ] as const) {
    const asks = await twoAsks();
    expect(removeAskMark(asks.view, asks[removed])).toBe(true);
    const doc = asks.view.state.doc;
    expect({ removed, text: markedText(doc, asks[removed]) }).toEqual({ removed, text: "" });
    expect({ kept, text: markedText(doc, asks[kept]) }).toEqual({ kept, text: keptText });
    expect(doc.textContent).toBe("The quick brown fox");
  }
});

test("removing an ask that is not in the document reports false and dispatches nothing", async () => {
  const { view, transactions } = await twoAsks();
  expect(removeAskMark(view, "never")).toBe(false);
  expect(transactions).toHaveLength(2);
});

test("an ask span nested inside another reads back from markdown as both asks", async () => {
  const { parseMarkdown, serializeMarkdown } = await createHeadlessProof();
  const ask = (id: string, by: string, text: string) =>
    `<span data-dispatch="ask" data-id="${id}" data-by="${by}">${text}</span>`;
  const markdown = `The ${ask("a1", "user:bob", `quick ${ask("a2", "user:alice", "brown")} fox`)} jumps.\n`;
  const doc = parseMarkdown(markdown);
  expect({ a1: markedText(doc, "a1"), a2: markedText(doc, "a2") }).toEqual({
    a1: "quick brown fox",
    a2: "brown",
  });
  const again = parseMarkdown(serializeMarkdown(doc));
  expect({ a1: markedText(again, "a1"), a2: markedText(again, "a2") }).toEqual({
    a1: "quick brown fox",
    a2: "brown",
  });
});
