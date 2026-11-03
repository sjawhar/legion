/**
 * The start of a stored text, cut out of the tree Go read it into rather than out of its
 * markdown. A markdown cut can land inside a mark and leave the delimiter it opened as literal
 * characters, which is why a cut source had to be balanced before it was parsed; a tree cut
 * drops no delimiter, because a mark is on the node and the node either stays or goes. The rule
 * for a cut the server makes is Go's (`pmdoc/fragment.go`); this is the one cut the browser
 * still makes, for a view that shows the head of a text it already holds whole.
 */

import type { DisplayTree } from "../../api/types";

/** A node's own characters, or `undefined` for a node that holds children instead. */
function textOf(node: DisplayTree): string | undefined {
  return typeof node.text === "string" ? node.text : undefined;
}

/** A node's children, or `undefined` for a node that holds none. */
function childrenOf(node: DisplayTree): readonly DisplayTree[] | undefined {
  const content = node.content;
  if (!Array.isArray(content)) {
    return undefined;
  }
  return content;
}

/** What a cut of one node left: the node to keep, absent where nothing of it fits, and how many
 *  characters of the budget are still free. */
interface CutNode {
  readonly node?: DisplayTree;
  readonly left: number;
}

/** How many characters of text a node holds, counted no further than `limit`: a node holding
 *  more than the budget answers `limit` or a little more, which is all the cut needs to know. */
function textLength(node: DisplayTree, limit: number): number {
  const text = textOf(node);
  if (text !== undefined) {
    return text.length;
  }
  let length = 0;
  for (const child of childrenOf(node) ?? []) {
    length += textLength(child, limit - length);
    if (length >= limit) {
      return length;
    }
  }
  return length;
}

/** One node cut to what is left of the budget: a text node whole where it fits and cut at its
 *  last whitespace otherwise, a node with children keeping the children that fit. The cut falls
 *  at the budget itself where the budget's second half holds no whitespace - one long URL or
 *  path - so the head still shows what it can, and never inside a surrogate pair. */
function cutNode(node: DisplayTree, left: number): CutNode {
  const text = textOf(node);
  if (text !== undefined) {
    if (text.length <= left) {
      return { left: left - text.length, node };
    }
    const space = /\s\S*$/u.exec(text.slice(0, left + 1))?.index ?? -1;
    let end = space >= left / 2 ? space : left;
    const last = text.charCodeAt(end - 1);
    if (last >= 0xd800 && last <= 0xdbff) {
      end -= 1;
    }
    const kept = text.slice(0, end).trimEnd();
    return { left: 0, node: kept === "" ? undefined : { ...node, text: kept } };
  }
  const children = childrenOf(node);
  if (children === undefined) {
    return { left, node };
  }
  const kept: DisplayTree[] = [];
  let remaining = left;
  for (const child of children) {
    const cut = cutNode(child, remaining);
    remaining = cut.left;
    if (cut.node !== undefined) {
      kept.push(cut.node);
    }
    if (remaining <= 0) {
      break;
    }
  }
  return { left: remaining, node: { ...node, content: kept } };
}

/** `node` with the ellipsis written onto the last text it holds, or `undefined` where it holds
 *  none: a cut that left one picture or one empty block takes no ellipsis, since there is
 *  nothing for it to follow. */
function closedWithEllipsis(node: DisplayTree): DisplayTree | undefined {
  const children = childrenOf(node);
  if (children === undefined) {
    return undefined;
  }
  for (let at = children.length - 1; at >= 0; at -= 1) {
    const child = children[at];
    const text = textOf(child);
    const closed = text === undefined ? closedWithEllipsis(child) : { ...child, text: `${text}…` };
    if (closed !== undefined) {
      const written = [...children];
      written[at] = closed;
      return { ...node, content: written };
    }
  }
  return undefined;
}

/**
 * The start of the stored text `tree` was read from, for a place that shows only its start: the
 * tree itself when it holds at most `max` characters of text (UTF-16 units), and otherwise the
 * nodes that fit, the last of them cut at its last whitespace and closed with an ellipsis.
 * Every mark, every attribute and every block the cut keeps is the server's own, so the head
 * draws exactly as the start of the whole text draws.
 */
export function treeHead(tree: DisplayTree, max: number): DisplayTree {
  if (textLength(tree, max + 1) <= max) {
    return tree;
  }
  const { node } = cutNode(tree, max);
  if (node === undefined) {
    return tree;
  }
  return closedWithEllipsis(node) ?? node;
}
