import { expect, test } from "bun:test";

import { issueAddress, pathToTarget, presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";

function node(id: string, type: NodeType, over: Partial<TreeNode> = {}): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    gaps: [],
    held: 0,
    id,
    question: false,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    ...over,
  };
}

const askNode = node("spec#b-q-open", "ask", {
  reference: "dispatch://CORE-1/ask/ask-9",
});
const commentNode = node("comment-7", "comment", {
  reference: "dispatch://CORE-1/comment/comment-7",
});
const tree = node("CORE-1", "issue", {
  children: [node("spec", "document", { children: [askNode] }), commentNode],
});

test("each preset's address is the one its tab carries", () => {
  expect(presetPath("CORE-1", "tree")).toBe("/issues/CORE-1");
  expect(presetPath("CORE-1", "checks")).toBe("/issues/CORE-1/verification");
  expect(presetPath("CORE-1", "spec")).toBe("/issues/CORE-1/spec");
});

test("the address says which preset it opens and which node it names", () => {
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "", false).preset).toBe("tree");
  expect(issueAddress({ key: "CORE-1", kind: "verification" }, "", "", false).preset).toBe(
    "checks"
  );
  // A route naming the issue's own spec by its slug is the Spec preset's address too.
  expect(
    issueAddress({ key: "CORE-1", kind: "artifact", slug: "spec" }, "?v=2", "", true).preset
  ).toBe("spec");
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "?ask=ask-9", "", false).target).toEqual({
    id: "ask-9",
    kind: "item",
  });
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-r3", false).target).toEqual({
    id: "r3",
    kind: "block",
  });
});

test("an item is found by its own id, and an ask by the address of the ask it was indexed from", () => {
  const byId = pathToTarget(tree, { id: "comment-7", kind: "item" });
  expect(byId?.map((found) => found.id)).toEqual(["CORE-1", "comment-7"]);

  const byReference = pathToTarget(tree, { id: "ask-9", kind: "item" });
  expect(byReference?.map((found) => found.id)).toEqual(["CORE-1", "spec", "spec#b-q-open"]);

  const byBlock = pathToTarget(tree, { id: "q-open", kind: "block" });
  expect(byBlock?.map((found) => found.id)).toEqual(["CORE-1", "spec", "spec#b-q-open"]);

  expect(pathToTarget(tree, { id: "nothing", kind: "item" })).toBeUndefined();
  expect(pathToTarget(tree, undefined)).toBeUndefined();
});
