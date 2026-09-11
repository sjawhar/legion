import { expect, test } from "bun:test";
import { Schema } from "prosemirror-model";

import { composerKindFor, markPlacements, setActiveMarkClass } from "./marks";

const schema = new Schema({
  marks: {
    dispatchAsk: { attrs: { by: {}, id: {} } },
    proofComment: { attrs: { by: {}, id: {} } },
  },
  nodes: {
    doc: { content: "paragraph+" },
    paragraph: { content: "inline*", group: "block" },
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
