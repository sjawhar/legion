import type { TreeNode } from "./issue-tree";
import type { DrawnRow, OpenedBranches } from "./tree-fold";

/**
 * What the tree's keyboard asks of the model (dispatch://INFRA-1196 R9,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t15`). Pure, over the tree `issue-tree.ts` builds
 * and the rows `tree-fold.ts` draws, so the keys hold no reading of the model of their own and
 * a unit test answers the same question the page does.
 */

/** What tells two rows apart: a node drawn under two requirements is two rows, and the focus,
 *  the body and the pin belong to the row rather than to the node they are on. */
export function rowKeyOf(row: DrawnRow): string {
  if (row.kind === "deeper") return `deeper:${row.of.id}`;
  return `${row.parent ?? ""}>${row.node.id}`;
}

/** Every node of `root` in reading order, the root first: the order the rows are drawn in, and
 *  the order a key looking for the next node of a kind walks, whatever is folded. */
export function nodesInOrder(root: TreeNode): TreeNode[] {
  const found: TreeNode[] = [];
  const walk = (node: TreeNode): void => {
    found.push(node);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return found;
}

/**
 * The next open question after `from`, in reading order, for a reader standing on one; from
 * anywhere else - no focus at all, or a node that is no question - the first open question of
 * the tree, because a position among nodes that are not questions says nothing about where in
 * the questions the reader is. Undefined when the reader is on the last one, which the key
 * reports rather than wrapping round to a question they have just read (R6).
 */
export function nextQuestion(root: TreeNode, from: string | undefined): TreeNode | undefined {
  const order = nodesInOrder(root);
  const at = order.findIndex((node) => node.id === from && node.question);
  return order.slice(at + 1).find((node) => node.question);
}

/** A reference with the version it pins taken off, so a decision's link to a block and that
 *  block's own address are read as the same node (`dispatch://KEY/spec#b-r1@v3`). */
function withoutVersion(reference: string): string {
  return reference.replace(/@v\d+/g, "");
}

/**
 * The decision behind the node `id`: the first decision of the tree, in reading order, that
 * names that node among its chips. A decision names every spec block it links
 * (`tree-drafts.ts`'s `addDecisions`), and that link is the only edge the model holds between a
 * decision and what it shaped until pull request 2797 lands its `shaped` edge; the day it does,
 * this reads that edge instead and the key does not change.
 */
export function decisionBehind(root: TreeNode, id: string): TreeNode | undefined {
  const order = nodesInOrder(root);
  const node = order.find((one) => one.id === id);
  if (node === undefined) return undefined;
  const addresses = new Set(
    [node.reference, node.id].filter((one): one is string => one !== undefined).map(withoutVersion)
  );
  return order.find(
    (one) =>
      one.type === "decision" && one.chips.some((chip) => addresses.has(withoutVersion(chip.ref)))
  );
}

/** Every branch of `root` opened, which is the whole tree drawn: the last state of the key that
 *  cycles how much of it is on screen. */
export function everythingOpened(root: TreeNode): OpenedBranches {
  const ids = nodesInOrder(root).map((node) => node.id);
  return { branches: new Set(ids), deeper: new Set(ids) };
}
