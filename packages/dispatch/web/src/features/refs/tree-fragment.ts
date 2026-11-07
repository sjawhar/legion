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

/**
 * The start of one run of characters: `text` itself where it is at most `max` characters
 * (UTF-16 units), and otherwise its last whitespace within `max`, with the space the cut left
 * trimmed off. Where the budget's second half holds no whitespace - one long URL or path - the
 * cut falls at `max` itself, and never inside a surrogate pair.
 *
 * This is the cut a text node takes below, and it is also the one a caller makes on the
 * markdown it hands `MarkdownPreview` beside a cut tree. That markdown is the preview's
 * fallback: without an engine, or for a tree the schema refuses, `useRenderedMarkdown` writes
 * it into the element as literal text, which a screen reader then reads as the row's whole
 * name. Bounding it keeps that fallback the same length as the tree the preview usually draws.
 */
export function textHead(text: string, max: number): string {
  if (text.length <= max) {
    return text;
  }
  const space = /\s\S*$/u.exec(text.slice(0, max + 1))?.index ?? -1;
  let end = space >= max / 2 ? space : max;
  const last = text.charCodeAt(end - 1);
  if (last >= 0xd800 && last <= 0xdbff) {
    end -= 1;
  }
  return text.slice(0, end).trimEnd();
}

/** One node cut to what is left of the budget: a text node whole where it fits and cut by
 *  `textHead` otherwise, a node with children keeping the children that fit. */
function cutNode(node: DisplayTree, left: number): CutNode {
  const text = textOf(node);
  if (text !== undefined) {
    if (text.length <= left) {
      return { left: left - text.length, node };
    }
    const kept = textHead(text, left);
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
