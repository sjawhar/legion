import { expect, test } from "bun:test";
import { Schema } from "prosemirror-model";

import {
  blockPlacements,
  composerKindFor,
  markPlacements,
  setActiveBlockClass,
  setActiveMarkClass,
} from "./marks";

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

test("composerKindFor maps every selection-bar composer action", () => {
  expect(composerKindFor("comment")).toBe("comment");
  expect(composerKindFor("suggest")).toBe("suggestion");
  expect(composerKindFor("ask")).toBe("ask");
});

test("setActiveMarkClass toggles the active class on matching spans only", () => {
  const root = document.createElement("div");
  root.innerHTML = '<span data-id="a"></span><span data-id="b"></span><span></span>';

  setActiveMarkClass(root, ["a"]);

  expect(root.querySelector('[data-id="a"]')?.classList.contains("dispatch-mark-active")).toBe(
    true
  );
  expect(root.querySelector('[data-id="b"]')?.classList.contains("dispatch-mark-active")).toBe(
    false
  );

  setActiveMarkClass(root, []);

  expect(root.querySelector('[data-id="a"]')?.classList.contains("dispatch-mark-active")).toBe(
    false
  );
});

test("setActiveBlockClass toggles the active class on matching block elements only", () => {
  const root = document.createElement("div");
  root.innerHTML = '<p data-block-id="a"></p><p data-block-id="b"></p>';

  setActiveBlockClass(root, ["a"]);

  expect(
    root.querySelector('[data-block-id="a"]')?.classList.contains("dispatch-block-active")
  ).toBe(true);
  expect(
    root.querySelector('[data-block-id="b"]')?.classList.contains("dispatch-block-active")
  ).toBe(false);
});
