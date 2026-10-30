import { expect, test } from "bun:test";

import {
  type BlockSpans,
  carriedBlocks,
  mapSpans,
  spansFromBlocks,
  type TextEditRange,
} from "./block-spans";

/** The blocks route's answer for a body, from each block's text: its id and where the text starts
 *  and ends in the body, in UTF-16 code units. */
function blocksOf(body: string, ...blocks: [id: string, text: string][]) {
  let from = 0;
  return blocks.map(([id, text]) => {
    from = body.indexOf(text, from);
    const block = { from, id, text, to: from + text.length };
    from = block.to;
    return block;
  });
}

function spansOf(body: string, ...blocks: [id: string, text: string][]): BlockSpans {
  const spans = spansFromBlocks(body, body, blocksOf(body, ...blocks));
  if (spans === undefined) throw new Error("expected the blocks to be the body's");
  return spans;
}

/** One textarea edit: `range` of the value replaced by `text`, as the browser reports it. */
function edit(
  spans: BlockSpans,
  value: string,
  range: TextEditRange,
  text: string,
  inputType: string
): { spans: BlockSpans; value: string } {
  const after = `${value.slice(0, range.start)}${text}${value.slice(range.end)}`;
  return { spans: mapSpans(spans, value, after, range, inputType), value: after };
}

function carried(state: { spans: BlockSpans; value: string }) {
  return carriedBlocks(state.spans, state.value, state.value.trim());
}

const twoBlocks = "Alpha\n\nBeta";

test("typing inside a block grows its span and moves the blocks after it", () => {
  const body = "Alpha\n\nBeta\n\nGamma";
  const typed = edit(
    spansOf(body, ["b1", "Alpha"], ["b2", "Beta"], ["b3", "Gamma"]),
    body,
    { end: 9, start: 9 },
    "xx",
    "insertText"
  );

  expect(typed.value).toBe("Alpha\n\nBexxta\n\nGamma");
  expect(typed.spans.live).toEqual([
    { end: 5, id: "b1", start: 0 },
    { end: 13, id: "b2", start: 7 },
    { end: 20, id: "b3", start: 15 },
  ]);
  expect(carried(typed)).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b2" },
    { from: 15, id: "b3" },
  ]);
});

test("Enter in the middle of a block keeps its start, and a paragraph typed above a block is not it", () => {
  let state = { spans: spansOf(twoBlocks, ["b1", "Alpha"], ["b2", "Beta"]), value: twoBlocks };
  state = edit(state.spans, state.value, { end: 9, start: 9 }, "\n", "insertLineBreak");
  state = edit(state.spans, state.value, { end: 10, start: 10 }, "\n", "insertLineBreak");

  expect(state.value).toBe("Alpha\n\nBe\n\nta");
  // The server splits "Be" from "ta"; the part holding b2's start keeps b2.
  expect(state.spans.live).toEqual([
    { end: 5, id: "b1", start: 0 },
    { end: 13, id: "b2", start: 7 },
  ]);
  expect(carried(state)).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b2" },
  ]);

  const above = edit(state.spans, state.value, { end: 7, start: 7 }, "New\n\n", "insertText");
  expect(above.value).toBe("Alpha\n\nNew\n\nBe\n\nta");
  expect(carried(above)).toEqual([
    { from: 0, id: "b1" },
    { from: 12, id: "b2" },
  ]);
});

test("joining two blocks with Backspace leaves both starts in one block", () => {
  let state = { spans: spansOf(twoBlocks, ["b1", "Alpha"], ["b2", "Beta"]), value: twoBlocks };
  state = edit(state.spans, state.value, { end: 7, start: 6 }, "", "deleteContentBackward");
  state = edit(state.spans, state.value, { end: 6, start: 5 }, "", "deleteContentBackward");

  expect(state.value).toBe("AlphaBeta");
  // The server finds both in one block and keeps the first, b1.
  expect(carried(state)).toEqual([
    { from: 0, id: "b1" },
    { from: 5, id: "b2" },
  ]);
});

test("deleting a block's whole text removes its span", () => {
  const body = "Alpha\n\nBeta\n\nGamma";
  const deleted = edit(
    spansOf(body, ["b1", "Alpha"], ["b2", "Beta"], ["b3", "Gamma"]),
    body,
    { end: 13, start: 7 },
    "",
    "deleteContentBackward"
  );

  expect(deleted.value).toBe("Alpha\n\nGamma");
  expect(deleted.spans).toEqual({
    held: [],
    live: [
      { end: 5, id: "b1", start: 0 },
      { end: 12, id: "b3", start: 7 },
    ],
  });
  expect(carried(deleted)).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b3" },
  ]);

  // Deleting part of a block's text keeps the block, starting where its text now starts.
  const partly = edit(
    deleted.spans,
    deleted.value,
    { end: 9, start: 5 },
    "",
    "deleteContentBackward"
  );
  expect(partly.value).toBe("Alphamma");
  expect(partly.spans.live).toEqual([
    { end: 5, id: "b1", start: 0 },
    { end: 8, id: "b3", start: 5 },
  ]);
});

test("typing over a block's whole text rewords it, and typing over several blocks removes them", () => {
  const body = "Alpha\n\nBeta\n\nGamma";
  const spans = spansOf(body, ["b1", "Alpha"], ["b2", "Beta"], ["b3", "Gamma"]);
  // The first key typed over the selected block replaces its whole text; the next one grows it.
  let state = edit(spans, body, { end: 11, start: 7 }, "O", "insertText");
  state = edit(state.spans, state.value, { end: 8, start: 8 }, "ther", "insertText");

  expect(state.value).toBe("Alpha\n\nOther\n\nGamma");
  expect(state.spans.live).toEqual([
    { end: 5, id: "b1", start: 0 },
    { end: 12, id: "b2", start: 7 },
    { end: 19, id: "b3", start: 14 },
  ]);
  expect(carried(state)).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b2" },
    { from: 14, id: "b3" },
  ]);

  // A paste over the selected block is a rewording too; words pasted over two blocks are neither.
  const pasted = edit(spans, body, { end: 11, start: 7 }, "\n\nPasted", "insertFromPaste");
  expect(pasted.spans.live).toContainEqual({ end: 15, id: "b2", start: 9 });
  const overTwo = edit(spans, body, { end: 18, start: 7 }, "Both", "insertText");
  expect(overTwo.value).toBe("Alpha\n\nBoth");
  expect(overTwo.spans.live).toEqual([{ end: 5, id: "b1", start: 0 }]);
});

test("a block cut and pasted elsewhere keeps its id", () => {
  const body = "Alpha\n\nBeta\n\nGamma";
  let state = {
    spans: spansOf(body, ["b1", "Alpha"], ["b2", "Beta"], ["b3", "Gamma"]),
    value: body,
  };
  state = edit(state.spans, state.value, { end: 13, start: 7 }, "", "deleteByCut");
  expect(state.value).toBe("Alpha\n\nGamma");
  expect(state.spans.held).toEqual([{ cut: "Beta\n\n", id: "b2", offset: 0, text: "Beta" }]);
  expect(carried(state)).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b3" },
  ]);

  state = edit(state.spans, state.value, { end: 12, start: 12 }, "\n\n", "insertText");
  state = edit(state.spans, state.value, { end: 14, start: 14 }, "Beta\n\n", "insertFromPaste");
  expect(state.value).toBe("Alpha\n\nGamma\n\nBeta\n\n");
  expect(state.spans.held).toEqual([]);
  expect(carried(state)).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b3" },
    { from: 14, id: "b2" },
  ]);

  // A second paste of the same text is a copy: the id went back with the first.
  const again = edit(state.spans, state.value, { end: 0, start: 0 }, "Beta\n\n", "insertFromPaste");
  expect(carried(again)?.map((block) => block.id)).toEqual(["b1", "b3", "b2"]);
});

test("a block dragged elsewhere keeps its id, and one deleted otherwise is not held", () => {
  let state = { spans: spansOf(twoBlocks, ["b1", "Alpha"], ["b2", "Beta"]), value: twoBlocks };
  state = edit(state.spans, state.value, { end: 5, start: 0 }, "", "deleteByDrag");
  state = edit(state.spans, state.value, { end: 6, start: 6 }, "\n\nAlpha", "insertFromDrop");
  expect(state.value).toBe("\n\nBeta\n\nAlpha");
  expect(carried(state)).toEqual([
    { from: 0, id: "b2" },
    { from: 6, id: "b1" },
  ]);

  const typedAway = edit(
    state.spans,
    state.value,
    { end: 13, start: 8 },
    "",
    "deleteContentBackward"
  );
  const typedBack = edit(
    typedAway.spans,
    typedAway.value,
    { end: 8, start: 8 },
    "Alpha",
    "insertFromPaste"
  );
  expect(carried(typedBack)).toEqual([{ from: 0, id: "b2" }]);
});

test("a CR LF body's offsets move into the textarea's LF value", () => {
  const stored = "Alpha\r\n\r\nBeta\r\nmore\r\n\r\nGamma";
  const value = "Alpha\n\nBeta\nmore\n\nGamma";
  const blocks = blocksOf(stored, ["b1", "Alpha"], ["b2", "Beta\r\nmore"], ["b3", "Gamma"]);
  expect(blocks.map((block) => [block.from, block.to])).toEqual([
    [0, 5],
    [9, 19],
    [23, 28],
  ]);

  const spans = spansFromBlocks(stored, value, blocks);
  expect(spans?.live).toEqual([
    { end: 5, id: "b1", start: 0 },
    { end: 16, id: "b2", start: 7 },
    { end: 23, id: "b3", start: 18 },
  ]);
  expect(spans === undefined ? undefined : carriedBlocks(spans, value, value)).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b2" },
    { from: 18, id: "b3" },
  ]);

  // Blocks of another body - the comment changed after the editor opened - carry nothing.
  expect(spansFromBlocks(stored, "Alpha\n\nBeta", blocks)).toBeUndefined();
  expect(
    spansFromBlocks(stored, value, [{ from: 0, id: "b1", text: "Alpine", to: 5 }])
  ).toBeUndefined();
});

test("the carried starts move back by the whitespace the save trims", () => {
  const typed = edit(
    spansOf(twoBlocks, ["b1", "Alpha"], ["b2", "Beta"]),
    twoBlocks,
    { end: 0, start: 0 },
    "  \n",
    "insertText"
  );
  const value = `${typed.value} \n\n`;
  expect(value).toBe("  \nAlpha\n\nBeta \n\n");
  const spans = mapSpans(typed.spans, typed.value, value, { end: 14, start: 14 }, "insertText");

  expect(carriedBlocks(spans, value, "Alpha\n\nBeta")).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b2" },
  ]);
  // A start outside the trimmed body is dropped.
  expect(
    carriedBlocks(
      { held: [], live: [...spans.live, { end: 16, id: "b3", start: 15 }] },
      value,
      "Alpha\n\nBeta"
    )
  ).toEqual([
    { from: 0, id: "b1" },
    { from: 7, id: "b2" },
  ]);
  // Spans that are not offsets into the body sent carry nothing.
  expect(carriedBlocks(spans, value, "Alpha\n\nBeta and more")).toBeUndefined();
});

test("an emoji before a block counts as two UTF-16 units", () => {
  const body = "😀 Alpha\n\nBeta";
  const blocks = blocksOf(body, ["b1", "😀 Alpha"], ["b2", "Beta"]);
  expect(blocks.map((block) => block.from)).toEqual([0, 10]);
  const spans = spansFromBlocks(body, body, blocks);
  if (spans === undefined) throw new Error("expected the blocks to be the body's");

  const typed = edit(spans, body, { end: 2, start: 2 }, "🎉", "insertText");
  expect(typed.value).toBe("😀🎉 Alpha\n\nBeta");
  expect(carried(typed)).toEqual([
    { from: 0, id: "b1" },
    { from: 12, id: "b2" },
  ]);

  // Backspace after the emoji: the caret's range names one unit, the browser deletes the pair,
  // and the spans follow what was deleted.
  const after = "😀 Alpha\n\nBeta";
  const deleted = mapSpans(
    typed.spans,
    typed.value,
    after,
    { end: 4, start: 3 },
    "deleteContentBackward"
  );
  expect(carriedBlocks(deleted, after, after)).toEqual([
    { from: 0, id: "b1" },
    { from: 10, id: "b2" },
  ]);

  // A start inside a surrogate pair moves to the pair's start.
  expect(
    carriedBlocks({ held: [], live: [{ end: 3, id: "b1", start: 1 }] }, "😀ab", "😀ab")
  ).toEqual([{ from: 0, id: "b1" }]);
});
