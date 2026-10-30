/**
 * Hoists a link out of the link label around it, on markdown import.
 *
 * CommonMark forbids an inline link inside a link label — `[foo [bar](/uri)](/uri2)` is read as
 * one link over the text `foo [bar` — so the only document that nests a `link` mdast node inside
 * another is a label holding an autolink: `[a <https://u.test> b](u)`.
 *
 * `@milkdown/transformer` holds one flat mark set while it parses (`lib/index.js:116-122`), so it
 * cannot read that label as written. The link mark's parser runner opens a mark, parses the
 * label's children and closes it again (`@milkdown/preset-commonmark/lib/index.js:350-360`);
 * `openMark` adds through ProseMirror's `Mark.addToSet`, so the inner autolink's link correctly
 * replaces the label's on the autolink's own text, but `closeMark` then removes the `link` *type*
 * with `removeFromSet` and takes the label's entry with it. Every run after the inner autolink was
 * left with no link at all, where Go's reader of the same document
 * (`packages/envoy/internal/dispatch/pmdoc/parse.go`) gives it the label's link again.
 *
 * This transform rewrites the nesting away before the schema runners ever see it: the inner link
 * becomes a sibling of the runs around it, and each of those runs is wrapped in its own copy of
 * the outer link, so `link(u)[text 'a ', link(inner)[text], text ' b']` becomes
 * `link(u)[text 'a '], link(inner)[text], link(u)[text ' b']`. That is the spelling Go's renderer
 * writes for the same tree (`[a ](u)<https://u.test>[ b](u)`), so this makes the two readers agree
 * rather than inventing a third reading. A wrapper between the two links — emphasis, strong,
 * strikethrough — is split the same way and copied around each piece, so the marks a run carries
 * are unchanged but for the link.
 *
 * Import only, in the sense that matters: a unified transformer runs on parse
 * (`ParserState.create` calls `remark.runSync`, `@milkdown/transformer/lib/index.js:160`) and
 * never on `stringify` (`SerializerState.toString` calls `remark.stringify` alone, line 374), so
 * registering it on the editor's shared remark processor cannot change what the editor writes.
 */

import { $remark } from '@milkdown/kit/utils';
import type { Root } from '@milkdown/kit/transformer';
import type { Transformer } from 'unified';

type MdastNode = {
  type: string;
  url?: string;
  title?: string | null;
  children?: MdastNode[];
};

/**
 * One part of a split label. A `hoisted` part stands on its own where the outer link stood; any
 * other part is content of the label, to be wrapped in a copy of the outer link.
 */
interface Piece {
  hoisted: boolean;
  node: MdastNode;
}

/** The outer link again over `children`: its own properties are `url` and `title`. */
function linkOver(link: MdastNode, children: MdastNode[]): MdastNode {
  return { type: 'link', url: link.url, title: link.title ?? null, children };
}

/**
 * Whether `node` holds a `link` below it, at any depth: the one thing `splitLink` can change
 * about a label. It allocates nothing, so a label holding none - every label of every comment
 * render, text/plain paste and `setMarkdown` import but the rare one around an autolink - costs
 * the walk alone. A node that holds one has children, which its type says, so a caller past this
 * test reaches them without a fallback.
 */
function holdsLink(node: MdastNode): node is MdastNode & { children: MdastNode[] } {
  if (node.children === undefined) return false;
  for (const child of node.children) {
    if (child.type === 'link' || holdsLink(child)) return true;
  }
  return false;
}

/**
 * The pieces of one split label grouped into the nodes that replace it, in order: each run of
 * pieces that stay in the label is flushed through `wrapRun`, and each hoisted piece through
 * `wrapHoisted`. A run is wrapped only when it has content, so a hoisted piece first or last in
 * the label leaves no empty wrapper behind. `splitLink` and `splitInLabel` group alike and differ
 * in the two wrappers alone, so the flush rule is written once here.
 */
function regroup<T>(
  pieces: Piece[],
  wrapRun: (run: MdastNode[]) => T,
  wrapHoisted: (node: MdastNode) => T
): T[] {
  const grouped: T[] = [];
  let run: MdastNode[] = [];
  for (const piece of pieces) {
    if (!piece.hoisted) {
      run.push(piece.node);
      continue;
    }
    if (run.length > 0) {
      grouped.push(wrapRun(run));
      run = [];
    }
    grouped.push(wrapHoisted(piece.node));
  }
  if (run.length > 0) grouped.push(wrapRun(run));
  return grouped;
}

/**
 * The siblings that replace `link`: the link itself when its label holds no link (`holdsLink`,
 * asked first so such a label allocates nothing), else its label's parts grouped (`regroup`),
 * with each run of label content wrapped in a copy of the link and each hoisted part standing on
 * its own.
 */
function splitLink(link: MdastNode): MdastNode[] {
  if (!holdsLink(link)) return [link];
  // Past `holdsLink` a piece is always hoisted: a label child that is a link is a hoisted piece,
  // and a child holding one is split into parts of which at least one is hoisted.
  return regroup(
    link.children.flatMap(splitInLabel),
    (run) => linkOver(link, run),
    (hoisted) => hoisted
  );
}

/**
 * The parts `node` contributes to the label it sits in. A link is hoisted, itself split first so
 * any depth of nesting comes out flat; a node holding one is split in its place and copied around
 * each part, so the hoisted link keeps the emphasis or strong around it and the label's content
 * keeps it too. A node holding no link is one part, untouched and shared rather than copied.
 */
function splitInLabel(node: MdastNode): Piece[] {
  if (node.type === 'link') {
    return splitLink(node).map((sibling) => ({ hoisted: true, node: sibling }));
  }
  if (node.children === undefined) return [{ hoisted: false, node }];
  const pieces = node.children.flatMap(splitInLabel);
  if (!pieces.some((piece) => piece.hoisted)) return [{ hoisted: false, node }];
  return regroup<Piece>(
    pieces,
    (run) => ({ hoisted: false, node: { ...node, children: run } }),
    (hoisted) => ({ hoisted: true, node: { ...node, children: [hoisted] } })
  );
}

/** Replaces every link of the tree with its own siblings; `splitLink` reaches inside each one. */
function hoist(node: MdastNode): void {
  const original = node.children;
  if (original === undefined) return;
  // Children are copied only once a child is replaced: up to that point the walk holds no array
  // at all, and a node whose links all stand keeps the array it came in with.
  let children: MdastNode[] | null = null;
  for (let index = 0; index < original.length; index += 1) {
    const child = original[index];
    if (child.type !== 'link') {
      hoist(child);
      children?.push(child);
      continue;
    }
    // `splitLink` reaches inside the link's own label, so a link child is never walked into.
    const siblings = splitLink(child);
    if (siblings.length === 1 && siblings[0] === child) {
      children?.push(child);
      continue;
    }
    if (children === null) children = original.slice(0, index);
    children.push(...siblings);
  }
  if (children !== null) node.children = children;
}

/** remark transformer — every parse seam registers it; `stringify` never runs it. */
export function remarkHoistNestedLinks() {
  return (tree: MdastNode) => {
    hoist(tree);
  };
}

export const libraryRemarkNestedLinksPlugin = $remark(
  'remarkHoistNestedLinks',
  () => remarkHoistNestedLinks as () => Transformer<Root, Root>,
);
