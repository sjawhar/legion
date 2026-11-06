import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import {
  decisionBehind,
  everythingOpened,
  nextQuestion,
  nodesInOrder,
  rowKeyOf,
} from "./tree-keys";

/**
 * What the tree's keyboard asks of the model (dispatch://INFRA-1196 R9,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t15`), over a tree built by hand.
 */

function node(
  id: string,
  type: NodeType,
  children: readonly TreeNode[] = [],
  extra: Partial<TreeNode> = {}
): TreeNode {
  return {
    alsoUnder: [],
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: children.reduce((total, child) => total + child.held + 1, 0),
    id,
    question: false,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
    ...extra,
  };
}

/** An issue whose spec holds two open questions and one answered one, under a document the
 *  reader has not opened, with a decision naming the first requirement. */
function issue(): TreeNode {
  return node("CORE-1", "issue", [
    node("spec", "document", [
      node("spec#b-q1", "ask", [], { question: true, reference: "dispatch://CORE-1/ask/one" }),
      node("spec#b-r1", "requirement", [], { reference: "dispatch://CORE-1/spec#b-r1" }),
      node("spec#b-q2", "ask", [], { question: true }),
      node("spec#b-answered", "ask"),
    ]),
    node("decisions-md#b-d1", "decision", [], {
      chips: [{ gone: false, name: "R1", ref: "dispatch://CORE-1/spec#b-r1@v3" }],
    }),
    node("decisions-md#b-d2", "decision", [], {
      chips: [{ gone: false, name: "R1", ref: "dispatch://CORE-1/spec#b-r1" }],
    }),
  ]);
}

test("the nodes read in the order the rows are drawn, the root first", () => {
  expect(nodesInOrder(issue()).map((one) => one.id)).toEqual([
    "CORE-1",
    "spec",
    "spec#b-q1",
    "spec#b-r1",
    "spec#b-q2",
    "spec#b-answered",
    "decisions-md#b-d1",
    "decisions-md#b-d2",
  ]);
});

test("the next-question key walks the open questions and reports past the last", () => {
  const root = issue();
  // From nowhere, and from a node that is no question, the key enters at the first question.
  expect(nextQuestion(root, undefined)?.id).toBe("spec#b-q1");
  expect(nextQuestion(root, "decisions-md#b-d1")?.id).toBe("spec#b-q1");
  // From a question, the next one; from the last, nothing, which the key reports.
  expect(nextQuestion(root, "spec#b-q1")?.id).toBe("spec#b-q2");
  expect(nextQuestion(root, "spec#b-q2")).toBeUndefined();
  // An answered question is no stop on the walk.
  expect(nextQuestion(node("CORE-1", "issue", [node("a", "ask")]), undefined)).toBeUndefined();
});

test("the decision behind a node is the first that names it, whatever version it pins", () => {
  const root = issue();
  expect(decisionBehind(root, "spec#b-r1")?.id).toBe("decisions-md#b-d1");
  expect(decisionBehind(root, "spec#b-q2")).toBeUndefined();
  expect(decisionBehind(root, "nothing-of-this-issue")).toBeUndefined();
});

test("the last state of the cycle opens every branch of the tree", () => {
  const opened = everythingOpened(issue());
  expect(opened.branches.has("spec")).toBe(true);
  expect(opened.deeper.has("spec")).toBe(true);
  expect(opened.branches.size).toBe(nodesInOrder(issue()).length);
});

test("a row is told from another by the node it holds and the parent it sits under", () => {
  const one = node("spec#b-r1", "requirement");
  expect(rowKeyOf({ kind: "node", level: 2, node: one, parent: "CORE-1" })).toBe(
    "CORE-1>spec#b-r1"
  );
  expect(rowKeyOf({ kind: "node", level: 1, node: one })).toBe(">spec#b-r1");
  expect(rowKeyOf({ held: 3, kind: "deeper", level: 4, of: one })).toBe("deeper:spec#b-r1");
});
