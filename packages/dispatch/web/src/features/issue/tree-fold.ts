import type { TreeNode } from "./issue-tree";

/**
 * How much of the issue's tree is drawn (dispatch://INFRA-1196 R1, R6). Pure, over the model
 * `issue-tree.ts` builds, so the rows, the zoom and the unit tests fold the same way.
 *
 * The page opens folded to the frontier and not to a fixed depth: the path to every open
 * question that waits on the reader is unfolded whatever its depth, and every other branch is
 * folded to its first level, saying how many nodes it holds. Those unfolded rows are not counted
 * against the fold, because there is no budget to count them against: a branch is folded unless
 * the frontier or the reader opened it, so a question's path costs no other branch a row.
 *
 * ## What a branch is
 *
 * A branch is a node that holds a set of its own: a document and its blocks, a child issue, a
 * comment or a message and its replies, and a decision that was replaced. Those fold, because
 * what they hold is a set the reader opens, and a reader who opened an issue did not ask for
 * every block of every document it carries.
 *
 * The reasoning is not a branch and is drawn: a requirement with the checks and plan tasks that
 * serve it, a check with its results, a decision with what it settled. That is what the page is
 * for, so it is on screen without a reader opening anything, down to the three drawn levels.
 *
 * Three levels are drawn. A branch deeper than that folds to one row saying how many nodes it
 * holds, which opens them in place; the frontier outranks that, since R6 unfolds a question's
 * path whatever the fold says.
 */

/** How many levels of the tree are drawn before a deeper branch folds to one row. */
export const DRAWN_LEVELS = 3;

/** The node kinds whose contents are a set the reader opens rather than the reasoning: these
 *  fold to one row with a count, and everything else is drawn. */
const FOLDED_KINDS: readonly string[] = ["child_issue", "comment", "document", "image", "message"];

/** What the reader has opened by hand: the branches they unfolded, and the deeper branches they
 *  opened in place. */
export interface OpenedBranches {
  /** The nodes whose children the reader unfolded. */
  readonly branches: ReadonlySet<string>;
  /** The nodes whose branch below `DRAWN_LEVELS` the reader opened in place. */
  readonly deeper: ReadonlySet<string>;
}

/** One row of the tree as the fold leaves it: the node, how deep it sits, and what its own
 *  twisty says. */
export interface TreeRow {
  /** `true` when its children are drawn, `false` when it holds children and they are not, and
   *  undefined for a node that holds none. */
  readonly expanded?: boolean;
  /** How many nodes the row holds while it is folded; undefined while it is not. */
  readonly folded?: number;
  readonly level: number;
  readonly node: TreeNode;
  /** The node this row sits under; undefined for the root. */
  readonly parent?: string;
}

/** One row standing for a branch below the drawn levels, which opens it in place. */
export interface DeeperRow {
  /** How many nodes the branch holds. */
  readonly held: number;
  readonly level: number;
  /** The node whose branch it stands for. */
  readonly of: TreeNode;
}

/** Every row the tree draws, in reading order: a node's own row, or the one row a branch below
 *  the drawn levels folds to. */
export type DrawnRow =
  | ({ readonly kind: "node" } & TreeRow)
  | ({ readonly kind: "deeper" } & DeeperRow);

/** Nothing opened by hand, which is how the page opens. */
export const NOTHING_OPENED: OpenedBranches = { branches: new Set(), deeper: new Set() };

/**
 * The ids of every node above an open question that waits on the reader: the path the fold
 * unfolds whatever its depth. The questions themselves are not in it - a question's own branch
 * is folded like any other - so a question that holds a deeper question is in it, as that one's
 * ancestor.
 */
export function frontier(root: TreeNode): ReadonlySet<string> {
  const path = new Set<string>();
  const walk = (node: TreeNode, above: readonly string[]): void => {
    if (node.question) {
      for (const id of above) path.add(id);
    }
    for (const child of node.children) walk(child, [...above, node.id]);
  };
  walk(root, []);
  return path;
}

/**
 * Every row the tree draws over `root`, in reading order. The root is always unfolded, because
 * an issue drawn as one folded row would say nothing at all; a branch below it is unfolded when
 * the frontier runs through it, when the reader opened it, or when it is the reasoning rather
 * than a set the reader opens.
 */
export function treeRows(root: TreeNode, opened: OpenedBranches = NOTHING_OPENED): DrawnRow[] {
  const path = frontier(root);
  const rows: DrawnRow[] = [];
  const walk = (node: TreeNode, level: number, parent: string | undefined): void => {
    const holds = node.children.length > 0;
    // A replaced decision's branch folds to the one line that settled it and opens by key: the
    // frontier does not open it, and nothing deletes it (R1).
    const foldsShut = node.replaced || FOLDED_KINDS.includes(node.type);
    // The reasoning is drawn and a set the reader opens is folded; how deep the drawn levels
    // reach is the rule below, not this one.
    const unfolded =
      level === 1 ||
      (holds &&
        (opened.branches.has(node.id) || (!node.replaced && path.has(node.id)) || !foldsShut));
    rows.push({
      ...(holds ? { expanded: unfolded } : {}),
      ...(holds && !unfolded ? { folded: node.held } : {}),
      kind: "node",
      level,
      node,
      ...(parent === undefined ? {} : { parent }),
    });
    if (!unfolded || !holds) return;
    // Past the drawn levels the branch is one row, unless the reader opened it in place or the
    // frontier runs through it.
    if (level >= DRAWN_LEVELS && !opened.deeper.has(node.id) && !path.has(node.id)) {
      rows.push({ held: node.held, kind: "deeper", level: level + 1, of: node });
      return;
    }
    for (const child of node.children) walk(child, level + 1, node.id);
  };
  walk(root, 1, undefined);
  return rows;
}

/**
 * The rows the tree draws at 1024 px and below, where it is read one node at a time: the node
 * the reader is on and its children as one line each, with no second indent level under them.
 * The path down to it is the breadcrumb above the rows (`zoomBreadcrumb`) rather than a row
 * apiece, because the identity block leaves a phone one screenful of rows, not five.
 */
export function zoomedRows(root: TreeNode, zoom: string | undefined): DrawnRow[] {
  const chain = pathTo(root, zoom ?? root.id) ?? [root];
  const node = chain.at(-1) ?? root;
  const rows: DrawnRow[] = [
    {
      ...(node.children.length > 0 ? { expanded: true } : {}),
      kind: "node",
      level: 1,
      node,
      ...(chain.length < 2 ? {} : { parent: chain[chain.length - 2]?.id ?? root.id }),
    },
  ];
  for (const child of node.children) {
    rows.push({
      ...(child.children.length > 0 ? { expanded: false, folded: child.held } : {}),
      kind: "node",
      level: 2,
      node: child,
      parent: node.id,
    });
  }
  return rows;
}

/** The nodes above the one the zoomed tree is on, outermost first: the breadcrumb a reader
 *  zooms back out through. Empty while the tree is on the issue itself. */
export function zoomBreadcrumb(root: TreeNode, zoom: string | undefined): TreeNode[] {
  return (pathTo(root, zoom ?? root.id) ?? [root]).slice(0, -1);
}

/** Which node the zoomed tree opens on: the node that holds the first open question waiting on
 *  the reader, so that question is on the first screen, and the issue itself when there is
 *  none. */
export function zoomOpensOn(root: TreeNode): string {
  const question = firstQuestion(root, [root]);
  return (question?.at(-2) ?? root).id;
}

/** The chain from the root down to `id`, the root first; undefined when the tree holds no such
 *  node. */
export function pathTo(root: TreeNode, id: string): TreeNode[] | undefined {
  if (root.id === id) return [root];
  for (const child of root.children) {
    const below = pathTo(child, id);
    if (below !== undefined) return [root, ...below];
  }
  return undefined;
}

/** The chain down to the first open question in reading order, the root first. */
function firstQuestion(node: TreeNode, above: readonly TreeNode[]): TreeNode[] | undefined {
  if (node.question) return [...above];
  for (const child of node.children) {
    const found = firstQuestion(child, [...above, child]);
    if (found !== undefined) return found;
  }
  return undefined;
}
