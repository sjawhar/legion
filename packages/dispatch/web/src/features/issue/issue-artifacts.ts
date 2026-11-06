import type { Artifact, IssueDetails } from "../../api/types";
import type { TreeNode } from "./issue-tree";

/**
 * Which artifact of the issue each node of the tree belongs to, and which artifacts no block
 * cites (dispatch://INFRA-1196 R16, `dispatch://INFRA-1196/artifact/plan-md#b-t17`,
 * `#b-t18`).
 *
 * A node's id is its own address inside the issue: an artifact is its slug, and a block of a
 * document is `<slug>#b-<block id>` (`tree-drafts.ts`'s `blockNodeId`). So the document a row
 * opens, and the block inside it the caret goes to, are read from the row's own id and the
 * issue's artifact list, with no further read.
 *
 * This module is pure. It holds no React and makes no request.
 */

/** Each artifact of the issue by its slug, which is how every node of the tree names one. */
export function artifactsBySlug(issue: IssueDetails): ReadonlyMap<string, Artifact> {
  return new Map(issue.artifacts.map((artifact) => [artifact.slug, artifact]));
}

/** One node's place in a document: the document itself, and the block of it the node is. */
export interface NodeDocument {
  readonly artifact: Artifact;
  /** The block of the document this node is, or `undefined` for the document's own node. */
  readonly blockId?: string;
}

/** The document `nodeId` belongs to, or `undefined` for a node that belongs to none: the issue
 *  itself, a child issue, a comment, a message, or a result. */
export function documentOfNode(
  nodeId: string,
  bySlug: ReadonlyMap<string, Artifact>
): NodeDocument | undefined {
  const cut = nodeId.indexOf("#b-");
  const slug = cut === -1 ? nodeId : nodeId.slice(0, cut);
  const artifact = bySlug.get(slug);
  if (artifact === undefined) return undefined;
  return cut === -1 ? { artifact } : { artifact, blockId: nodeId.slice(cut + 3) };
}

/**
 * The artifacts the node menu holds: every artifact of the issue the tree does not draw under
 * a block that cites it (R16). A document a block cites is a node under that block and opens
 * from there, so it is not in the menu; a document nothing cites, and an artifact that is no
 * document and so is no node at all, are.
 */
export function uncitedArtifacts(
  root: TreeNode,
  artifacts: readonly Artifact[]
): readonly Artifact[] {
  const cited = new Set<string>();
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (node.id !== root.id && (child.type === "document" || child.type === "image")) {
        cited.add(child.id);
      }
      walk(child);
    }
  };
  walk(root);
  return artifacts.filter((artifact) => !cited.has(artifact.slug));
}
