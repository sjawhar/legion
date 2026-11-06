import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import {
  DRAWN_LEVELS,
  type DrawnRow,
  frontier,
  treeRows,
  zoomBreadcrumb,
  zoomedRows,
} from "./tree-fold";

/**
 * The fold, over a tree built by hand (dispatch://INFRA-1196 R1, R6,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`): the page opens folded to the frontier and not
 * to a fixed depth, and a question's path costs no other branch a row.
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

/** The ids of every row drawn, in reading order; a branch's own row reads as `more:<id>`. */
function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => (row.kind === "deeper" ? `more:${row.of.id}` : row.node.id));
}

function rowOf(rows: readonly DrawnRow[], id: string): DrawnRow | undefined {
  return rows.find((row) => row.kind === "node" && row.node.id === id);
}

/** An issue whose open question sits `depth` levels below it, inside a chain of documents, with
 *  one other document of three blocks beside it. */
function issueWithQuestionAt(depth: number): TreeNode {
  let deepest = node("question", "ask", [], { question: true, state: "open" });
  for (let level = depth - 1; level > 0; level -= 1) {
    deepest = node(`doc-${level}`, "document", [deepest]);
  }
  const other = node("other", "document", [
    node("other#b-1", "requirement"),
    node("other#b-2", "requirement"),
    node("other#b-3", "requirement"),
  ]);
  return node("CORE-1", "issue", [deepest, other]);
}

test("the path to an open question is unfolded whatever its depth", () => {
  for (const depth of [1, 2, 6]) {
    const root = issueWithQuestionAt(depth);
    const rows = treeRows(root);
    const question = rowOf(rows, "question");
    expect(question, `no row for a question ${depth} levels down`).toBeDefined();
    expect(question?.kind === "node" ? question.level : 0).toBe(depth + 1);
    for (let level = 1; level < depth; level += 1) {
      const above = rowOf(rows, `doc-${level}`);
      expect(above?.kind === "node" ? above.expanded : undefined).toBe(true);
    }
  }
});

test("every other branch is folded to its first level with a count of what it holds", () => {
  const rows = treeRows(issueWithQuestionAt(2));
  const other = rowOf(rows, "other");
  expect(other?.kind === "node" ? other.expanded : undefined).toBe(false);
  expect(other?.kind === "node" ? other.folded : undefined).toBe(3);
  expect(idsOf(rows)).not.toContain("other#b-1");
});

test("a question's rows are not counted against the fold", () => {
  const withQuestion = issueWithQuestionAt(2);
  const answered = node("CORE-1", "issue", [
    node("doc-1", "document", [node("question", "ask", [], { state: "answered" })]),
    withQuestion.children[1] as TreeNode,
  ]);
  const before = idsOf(treeRows(withQuestion));
  const after = idsOf(treeRows(answered));
  expect(before).toContain("question");
  expect(after).not.toContain("question");
  // Nothing outside the question's own path moved: the other branch is folded either way.
  expect(after.filter((id) => !before.includes(id))).toEqual([]);
  expect(after.length).toBeLessThan(before.length);
});

test("the reasoning under a requirement is drawn without the reader opening anything", () => {
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [
      node("verification-md#b-c1", "check", [], { state: "passed" }),
    ]),
  ]);
  const rows = treeRows(root);
  expect(idsOf(rows)).toEqual(["CORE-1", "spec#b-r1", "verification-md#b-c1"]);
});

test("a branch below the drawn levels is one row saying how many nodes it holds", () => {
  const deep = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement", [
      node("verification-md#b-c1", "check", [
        node("result-1", "result"),
        node("result-2", "result"),
      ]),
    ]),
  ]);
  const rows = treeRows(deep);
  expect(idsOf(rows)).toEqual([
    "CORE-1",
    "spec#b-r1",
    "verification-md#b-c1",
    "more:verification-md#b-c1",
  ]);
  const more = rows.at(-1);
  expect(more?.kind === "deeper" ? more.held : 0).toBe(2);
  expect(more?.kind === "deeper" ? more.level : 0).toBe(DRAWN_LEVELS + 1);
  // The row above it says how many nodes it holds that no row of their own draws, as a row
  // whose own twisty is closed does: a reader of the tree pattern - c-keyboard's among them -
  // takes a row carrying `aria-expanded` to hold children, and these are drawn as one line.
  const branch = rowOf(rows, "verification-md#b-c1");
  expect(branch?.kind === "node" ? branch.folded : undefined).toBe(2);
  expect(branch?.kind === "node" ? branch.expanded : undefined).toBe(true);
  // Opened in place, the branch is drawn where it stands.
  const opened = treeRows(deep, {
    branches: new Set(),
    deeper: new Set(["verification-md#b-c1"]),
  });
  expect(idsOf(opened)).toContain("result-2");
});

test("a replaced decision folds to one line with a count, and is not deleted", () => {
  const root = node("CORE-1", "issue", [
    node("decisions-md#b-d1", "decision", [node("question", "ask", [], { question: true })], {
      replaced: true,
    }),
  ]);
  const rows = treeRows(root);
  const replaced = rowOf(rows, "decisions-md#b-d1");
  expect(replaced?.kind === "node" ? replaced.folded : undefined).toBe(1);
  expect(idsOf(rows)).not.toContain("question");
  // The reader opens it by hand and it is all there.
  const opened = treeRows(root, {
    branches: new Set(["decisions-md#b-d1"]),
    deeper: new Set(),
  });
  expect(idsOf(opened)).toContain("question");
});

test("the frontier holds every node above an open question and no question itself", () => {
  expect([...frontier(issueWithQuestionAt(2))].sort()).toEqual(["CORE-1", "doc-1"]);
});

test("the zoomed tree opens on the issue with the question's path unfolded under it", () => {
  const root = issueWithQuestionAt(2);
  // Nothing chosen is the issue itself, so every branch of it is one row away; the path down
  // to the open question is unfolded under it, at the one level the narrow tree has.
  const rows = zoomedRows(root, undefined);
  expect(idsOf(rows)).toEqual(["CORE-1", "doc-1", "question", "other"]);
  expect(zoomBreadcrumb(root, undefined)).toEqual([]);
  expect(rows.every((row) => row.level <= 2)).toBe(true);
  const unfolded = rowOf(rows, "doc-1");
  expect(unfolded?.kind === "node" ? unfolded.expanded : undefined).toBe(true);
  const other = rowOf(rows, "other");
  expect(other?.kind === "node" ? other.folded : undefined).toBe(3);
  // Zoomed into a node, the tree is that node and its children, under a breadcrumb that walks
  // back to the issue.
  const inside = zoomedRows(root, "doc-1");
  expect(idsOf(inside)).toEqual(["doc-1", "question"]);
  expect(zoomBreadcrumb(root, "doc-1").map((one) => one.id)).toEqual(["CORE-1"]);
  expect(inside.every((row) => row.level <= 2)).toBe(true);
});
