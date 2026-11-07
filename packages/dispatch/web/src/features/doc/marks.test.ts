import { expect, test } from "bun:test";
import { Schema } from "prosemirror-model";

import { blockOffsets, blockPlacements, composerKindFor, markPlacements } from "./marks";

const schema = new Schema({
  marks: {
    dispatchAsk: { attrs: { by: {}, id: {} } },
    proofComment: { attrs: { by: {}, id: {} } },
  },
  nodes: {
    doc: { content: "paragraph+" },
    paragraph: { attrs: { blockId: { default: null } }, content: "inline*", group: "block" },
    text: { group: "inline" },
  },
});

test("markPlacements reports the first position of every record-bearing mark", () => {
  const comment = schema.marks.proofComment.create({ by: "alice", id: "c-1" });
  const ask = schema.marks.dispatchAsk.create({ by: "bob", id: "a-1" });
  const doc = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [
      schema.text("The "),
      schema.text("quick", [comment]),
      schema.text(" brown "),
      schema.text("fox", [ask]),
      schema.text(" and "),
      schema.text("quick", [comment]),
    ]),
  ]);

  expect([
    ...markPlacements(
      doc,
      new Map([
        ["c-1", 40],
        ["a-1", 72],
      ])
    ),
  ]).toEqual([
    ["c-1", { pos: 5, top: 40 }],
    ["a-1", { pos: 17, top: 72 }],
  ]);
});

test("markPlacements reports the first position and matching vertical offset of every mark", () => {
  const comment = schema.marks.proofComment.create({ by: "alice", id: "c-1" });
  const ask = schema.marks.dispatchAsk.create({ by: "bob", id: "a-1" });
  const doc = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [
      schema.text("The "),
      schema.text("quick", [comment]),
      schema.text(" brown "),
      schema.text("fox", [ask]),
    ]),
  ]);

  expect([
    ...markPlacements(
      doc,
      new Map([
        ["c-1", 40],
        ["a-1", 72],
      ])
    ),
  ]).toEqual([
    ["c-1", { pos: 5, top: 40 }],
    ["a-1", { pos: 17, top: 72 }],
  ]);
});

test("blockPlacements reports each stable block's position and offset", () => {
  const doc = schema.node("doc", undefined, [
    schema.node("paragraph", { blockId: "block-1" }, schema.text("First")),
    schema.node("paragraph", { blockId: "block-2" }, schema.text("Second")),
  ]);

  expect([
    ...blockPlacements(
      doc,
      new Map([
        ["block-1", 40],
        ["block-2", 72],
      ])
    ),
  ]).toEqual([
    ["block-1", { pos: 0, top: 40 }],
    ["block-2", { pos: 7, top: 72 }],
  ]);
});

/** The editor's root holding two top-level blocks, each with a nested block inside it, and a
 *  count of how many boxes were read. happy-dom lays nothing out, so every box is the one
 *  stated here. */
function editorHolding(): { boxesRead: () => number; root: HTMLElement } {
  let read = 0;
  const box = (element: HTMLElement, top: number) => {
    element.getBoundingClientRect = () => {
      read += 1;
      return { top } as DOMRect;
    };
  };
  const root = document.createElement("div");
  box(root, 100);
  let top = 140;
  for (const [block, inside] of [
    ["block-1", "inside-1"],
    ["block-2", "inside-2"],
  ]) {
    const element = document.createElement("section");
    element.setAttribute("data-block-id", block);
    box(element, top);
    const nested = document.createElement("p");
    nested.setAttribute("data-block-id", inside);
    box(nested, top + 20);
    element.append(nested);
    root.append(element);
    top += 200;
  }
  return { boxesRead: () => read, root };
}

// A block inside a top-level block is answered with that block's own top, and its own box is
// never read: reading it would force a block the browser is skipping to render, one block at a
// time over the whole document (dispatch://INFRA-341, R11; the comment on `blockOffsets`).
test("blockOffsets reads one box per top-level block and places what is inside it there", () => {
  const { boxesRead, root } = editorHolding();

  expect([...blockOffsets(root, true)]).toEqual([
    ["block-1", 40],
    ["inside-1", 40],
    ["block-2", 240],
    ["inside-2", 240],
  ]);
  expect(boxesRead(), "a box was read for a block inside a block").toBe(3);
});

test("blockOffsets names every block and measures none while nothing is placed", () => {
  const { boxesRead, root } = editorHolding();

  expect([...blockOffsets(root, false)]).toEqual([
    ["block-1", 0],
    ["inside-1", 0],
    ["block-2", 0],
    ["inside-2", 0],
  ]);
  expect(boxesRead()).toBe(0);
});

test("composerKindFor maps every selection-bar composer action", () => {
  expect(composerKindFor("comment")).toBe("comment");
  expect(composerKindFor("suggest")).toBe("suggestion");
  expect(composerKindFor("ask")).toBe("ask");
});
