import { expect, test } from "bun:test";
import { withDomAttributes } from "../src/editor/schema/dom-attributes";

test("adds attributes without disturbing a DOM output spec's content hole", () => {
  expect(
    withDomAttributes(["section", { class: "proof-block" }, ["div", 0]], {
      "data-proof-block-attr-kind": "warning",
    })
  ).toEqual([
    "section",
    { class: "proof-block", "data-proof-block-attr-kind": "warning" },
    ["div", 0],
  ]);

  expect(
    withDomAttributes(["section", ["div", 0]], { "data-proof-block-attr-title": "Risk" })
  ).toEqual(["section", { "data-proof-block-attr-title": "Risk" }, ["div", 0]]);
});
