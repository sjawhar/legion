// How a whole parsed body is painted into an element: in one go, and a piece at a time for a
// body the parse worker parsed. Both painters live beside each other, and apart from the
// surfaces that use them, because three surfaces now share them: `MarkdownBody`, which draws a
// comment, ask or message body, and `features/doc/DocumentPreview.tsx`, which draws a whole
// document before its editor exists (dispatch://INFRA-341, R11).
import {
  type Attrs,
  type DOMOutputSpec,
  DOMSerializer,
  type Node as ProseMirrorNode,
} from "prosemirror-model";

import { drawTaskCheckboxes } from "./task-checkboxes";
import type { PaintMarkdown, SliceYield } from "./useRenderedMarkdown";

/**
 * The two passes over a whole painted body, after its nodes are in it and before anything reads
 * it: a task item's checkbox drawn, and a tight list item's paragraph unwrapped so the item
 * reads as one line rather than a paragraph of its own. One function, so the sliced painter and
 * the whole-body one cannot drift apart - a pass added to either reaches both.
 */
function finishBlock(element: HTMLElement): void {
  drawTaskCheckboxes(element);
  for (const item of element.querySelectorAll("li[data-spread='false']")) {
    const paragraph = item.firstElementChild;
    if (item.childElementCount === 1 && paragraph?.tagName === "P") {
      paragraph.replaceWith(...paragraph.childNodes);
    }
  }
}

export const paintBlock: PaintMarkdown = (element, parsed, serializer) => {
  element.replaceChildren(serializer.serializeFragment(parsed.content));
  finishBlock(element);
};

/**
 * `DOMSerializer.renderSpec` with the attribute object prosemirror-model's own serializer passes
 * it. Upstream's signature is `renderSpec(doc, structure, xmlNS = null, blockArraysIn)`, and
 * `blockArraysIn` is what makes it refuse a spec array that came out of an attribute object
 * (`suspiciousAttributes`), the guard a node's own `toDOM` is held to everywhere else.
 * prosemirror-model 1.25.11 declares only the first three in `dist/index.d.ts`, so the fourth is
 * passed through this one named type rather than at each call (sjawhar/legion#2519).
 */
const renderSpecWithAttrs = DOMSerializer.renderSpec as (
  doc: Document,
  structure: DOMOutputSpec,
  xmlNS: string | null,
  blockArraysIn: Attrs
) => { dom: HTMLElement; contentDOM?: HTMLElement };

/**
 * One node into `parent`, yielding where the work can be cut: a node holding several block
 * children - a list, a quote, the document itself - is opened with its own `toDOM`, as the
 * serializer would, and its children go in one at a time; everything else, a textblock and its
 * marks included, goes in whole, since splitting inline content would split a mark across
 * elements.
 *
 * An opened node is opened exactly as `DOMSerializer.serializeNode` opens it: its attributes go
 * to `renderSpec` as the guard wants them, and its own marks wrap what comes back, outermost
 * last, with the children still going into the node's own content hole.
 */
function* appendNode(
  parent: Node,
  node: ProseMirrorNode,
  serializer: DOMSerializer
): Generator<SliceYield> {
  const toDOM = serializer.nodes[node.type.name];
  if (node.isTextblock || node.childCount <= 1 || toDOM === undefined) {
    parent.appendChild(serializer.serializeNode(node));
    yield;
    return;
  }
  const { contentDOM, dom } = renderSpecWithAttrs(document, toDOM(node), null, node.attrs);
  let outermost: HTMLElement = dom;
  for (let at = node.marks.length - 1; at >= 0; at -= 1) {
    const mark = node.marks[at];
    const markToDOM = serializer.marks[mark.type.name];
    if (markToDOM === undefined) {
      continue;
    }
    const wrap = renderSpecWithAttrs(document, markToDOM(mark, node.isInline), null, mark.attrs);
    (wrap.contentDOM ?? wrap.dom).appendChild(outermost);
    outermost = wrap.dom;
  }
  parent.appendChild(outermost);
  if (contentDOM === undefined) {
    yield;
    return;
  }
  for (let child = 0; child < node.childCount; child += 1) {
    yield* appendNode(contentDOM, node.child(child), serializer);
  }
}

/**
 * How many nodes of the built body one task puts into the page. What an engine spends on a batch
 * is the style and layout of what has just arrived, in the rendering update after that task, so
 * the batch is what one such pass costs. Measured on this pod against the dashboard's own
 * stylesheet (sjawhar/legion#2519), an 8,000-item list: put in with one `replaceChildren` the
 * thread runs no task for 918-971 ms in WebKit, 211 ms in Chromium and 173 ms in Firefox; put in
 * 500 nodes at a time, 72 ms, 124 ms and 39 ms, because each engine then lays out only the items
 * of the batch. WebKit is why the batch exists at all: it spends ~700 ms on a pass over 16,000
 * freshly attached nodes whether their boxes are laid out or skipped, so nothing but splitting
 * the attach brings it under the second a reader may wait.
 */
const ATTACH_BATCH = 500;

/** How many nodes `node` holds, itself included, counted no further than `limit`: a subtree
 *  bigger than a batch answers `limit` or a little more, which is all a batch needs to know. */
function nodeCount(node: Node, limit: number): number {
  let count = 1;
  for (let child = node.firstChild; child !== null && count < limit; child = child.nextSibling) {
    count += nodeCount(child, limit - count);
  }
  return count;
}

/**
 * One built node into the page, batch by batch: a node no bigger than a batch goes in whole,
 * and a bigger one - the `<ul>` of an 8,000-item list - goes in as its own empty shell, its
 * children following it. The batch is shared across the whole attach, so a body of many blocks
 * fills as few tasks as a body of one, and a batch that is full ends its task (`"page"`).
 */
function* attachNode(parent: Node, node: Node, batch: { left: number }): Generator<SliceYield> {
  const size = nodeCount(node, ATTACH_BATCH + 1);
  if (size <= ATTACH_BATCH) {
    parent.appendChild(node);
    batch.left -= size;
    if (batch.left <= 0) {
      batch.left = ATTACH_BATCH;
      yield "page";
    }
    return;
  }
  // `appendChild` would carry the children in with it, so they wait off the page in a fragment
  // and follow one at a time; each one leaves the fragment as it is put in, which is what walks
  // the loop below.
  const waiting = document.createDocumentFragment();
  while (node.firstChild !== null) {
    waiting.appendChild(node.firstChild);
  }
  parent.appendChild(node);
  batch.left -= 1;
  if (batch.left <= 0) {
    batch.left = ATTACH_BATCH;
    yield "page";
  }
  while (waiting.firstChild !== null) {
    yield* attachNode(node, waiting.firstChild, batch);
  }
}

/**
 * `paintBlock`, a piece at a time (`PaintMarkdownInSlices`), for a body the parse worker parsed:
 * the 16,000 elements of an 8,000-item list are built across several tasks rather than in one,
 * and then put into the page a batch at a time (`ATTACH_BATCH`), so no single task holds the
 * reader's thread while the body lands.
 *
 * The build is off the page, exactly as `paintBlock` serializes a body off the page: a tree
 * built in the page would be styled and laid out again for every piece, and everything watching
 * the document - a `MutationObserver`, the DOM recorder a browser test runs under - would pay
 * for each of the 16,000 changes. What reaches the page is the finished tree, and the body's
 * finishing pass (`finishBlock`, the same one `paintBlock` runs) runs on it before it does, so
 * it costs one off-page walk rather than thousands of live mutations. Only the attach itself is
 * split, and by node count rather than by time: putting a node in is a pointer's work, and what
 * it costs is paid by the engine afterwards, where no clock of this module's can see it.
 */
export function* paintBlockInSlices(
  element: HTMLElement,
  parsed: ProseMirrorNode,
  serializer: DOMSerializer
): Generator<SliceYield> {
  const built = document.createElement("div");
  for (let block = 0; block < parsed.childCount; block += 1) {
    yield* appendNode(built, parsed.child(block), serializer);
  }
  finishBlock(built);
  yield;
  // The reader's own text goes as the first batch of the body arrives, so the element holds one
  // or the other at every moment the engine draws, and the batches fill it from there. What it
  // held waits off the page meanwhile and goes back if this paint is stopped before its last
  // batch, so a paint the next answer takes over from leaves the whole body the reader was
  // reading, never the prefix of one that was still arriving (sjawhar/legion#2519).
  const held = document.createDocumentFragment();
  while (element.firstChild !== null) {
    held.appendChild(element.firstChild);
  }
  let landed = false;
  try {
    const batch = { left: ATTACH_BATCH };
    while (built.firstChild !== null) {
      yield* attachNode(element, built.firstChild, batch);
    }
    landed = true;
  } finally {
    if (!landed) {
      element.replaceChildren(held);
    }
  }
}
