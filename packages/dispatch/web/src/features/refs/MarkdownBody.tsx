import { DOMSerializer, type Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useRef } from "react";

import { markdownClassName, type SoftBreaks } from "./markdown-engine";
import { drawTaskCheckboxes } from "./task-checkboxes";
import { type PaintMarkdown, type SliceYield, useRenderedMarkdown } from "./useRenderedMarkdown";

/** The first textblock's own inline content (marks intact), plus the flattened plain text of
 * every textblock after it, joined with a single space - see `flattenInline`. */
interface InlineContent {
  head: ProseMirrorNode;
  extra: string;
}

/** A textblock's own text, skipping any code span or code block content — used only for the
 * "extra" (non-first) textblocks `flattenInline` yields, which flatten to plain text and so
 * would otherwise re-expose a code span's `dispatch://` ref as linkifiable bare text once its
 * `code` mark is gone. The first textblock keeps its marks (serialized, not flattened), so its
 * own code spans stay real `<code>` elements and need no such filtering. */
function plainTextExcludingCode(node: ProseMirrorNode): string {
  const parts: string[] = [];
  node.descendants((child) => {
    if (child.type.name === "code_block") {
      return false;
    }
    if (child.isText) {
      const hasCode = child.marks.some((mark) => mark.type.name === "inlineCode");
      if (!hasCode) {
        parts.push(child.text ?? "");
      }
    }
    return true;
  });
  return parts.join("");
}

/** A parsed document collapsed onto one line: the first textblock's inline content verbatim
 * (marks intact) and every later textblock's plain text after it, space-joined, so a list or a
 * multi-paragraph body renders as one run of inline content instead of nesting a `<ul>` or a
 * second `<p>` inside a span. `undefined` for a document with no text at all. */
function flattenInline(root: ProseMirrorNode): InlineContent | undefined {
  let head: ProseMirrorNode | undefined;
  const extra: string[] = [];
  root.descendants((node) => {
    if (!node.isTextblock) {
      return true;
    }
    if (head === undefined) {
      head = node;
    } else {
      const text = plainTextExcludingCode(node).trim();
      if (text !== "") {
        extra.push(text);
      }
    }
    return false;
  });
  return head === undefined ? undefined : { extra: extra.join(" "), head };
}

const paintInline: PaintMarkdown = (element, parsed, serializer) => {
  const flattened = flattenInline(parsed);
  element.replaceChildren();
  if (flattened !== undefined) {
    element.appendChild(serializer.serializeFragment(flattened.head.content));
    if (flattened.extra !== "") {
      element.appendChild(document.createTextNode(` ${flattened.extra}`));
    }
  }
};

const paintBlock: PaintMarkdown = (element, parsed, serializer) => {
  element.replaceChildren(serializer.serializeFragment(parsed.content));
  drawTaskCheckboxes(element);
  unwrapTightItems(element);
};

/** A tight list item's paragraph (`li[data-spread='false']`) holding nothing else is unwrapped,
 *  so the item reads as one line rather than a paragraph of its own. */
function unwrapTightItems(element: HTMLElement): void {
  for (const item of element.querySelectorAll("li[data-spread='false']")) {
    const paragraph = item.firstElementChild;
    if (item.childElementCount === 1 && paragraph?.tagName === "P") {
      paragraph.replaceWith(...paragraph.childNodes);
    }
  }
}

/**
 * One node into `parent`, yielding where the work can be cut: a node holding several block
 * children - a list, a quote, the document itself - is opened with its own `toDOM`, as the
 * serializer would, and its children go in one at a time; everything else, a textblock and its
 * marks included, goes in whole, since splitting inline content would split a mark across
 * elements.
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
  const { contentDOM, dom } = DOMSerializer.renderSpec(document, toDOM(node));
  parent.appendChild(dom);
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
 * stylesheet (sjawhar/legion#2403), an 8,000-item list: put in with one `replaceChildren` the
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
 * for each of the 16,000 changes. What reaches the page is the finished tree, and the two passes
 * over the whole body (`drawTaskCheckboxes`, `unwrapTightItems`) run on it before it does, so
 * they cost one off-page walk rather than thousands of live mutations. Only the attach itself is
 * split, and by node count rather than by time: putting a node in is a pointer's work, and what
 * it costs is paid by the engine afterwards, where no clock of this module's can see it.
 */
function* paintBlockInSlices(
  element: HTMLElement,
  parsed: ProseMirrorNode,
  serializer: DOMSerializer
): Generator<SliceYield> {
  const built = document.createElement("div");
  for (let block = 0; block < parsed.childCount; block += 1) {
    yield* appendNode(built, parsed.child(block), serializer);
  }
  drawTaskCheckboxes(built);
  yield;
  unwrapTightItems(built);
  yield;
  // The reader's own text goes as the first batch of the body arrives, so the element holds one
  // or the other at every moment the engine draws, and the batches fill it from there.
  element.replaceChildren();
  const batch = { left: ATTACH_BATCH };
  while (built.firstChild !== null) {
    yield* attachNode(element, built.firstChild, batch);
  }
}

/**
 * Renders Markdown text through Proof's own parser and schema, so every question, answer,
 * comment, reply, and message formats identically to the document editor. `block` (the
 * default) renders the full parsed document, including lists and headings, inside a `<div>`;
 * there a task item (`- [x] done`) draws a disabled checkbox, ticked as its source is.
 * `inline` renders only inline marks (bold, code, links) inside a `<span>` with no wrapping
 * `<p>` or block spacing, for single-line contexts like an option label or a resolution reason
 * inside a metadata line:
 * it takes the first textblock's inline content verbatim (marks intact) and appends every
 * later textblock's plain text after it, space-joined, so a list or a multi-paragraph body
 * collapses onto one line instead of nesting a `<ul>`/second `<p>` inside the span. Both
 * variants inherit the surrounding text's size and color (`.dispatch-markdown` in styles.css
 * overrides Tailwind Typography's fixed palette, heading scale, and font size) so a heading
 * inside, say, an ask question reads as bold text at the card's own size rather than a
 * page-size h1, and a resolution reason inline in a `text-xs` line stays that size.
 *
 * A picture whose address is a Dispatch artifact at a version (`![shot.png](dispatch://…@v1)`,
 * what a pasted picture is written as) shows through `DispatchPicture`: at the column's width,
 * capped in height, in `block`; as a thumbnail beside its caption in `inline`. A picture on
 * another website renders as Proof renders it.
 *
 * Once the schema and Proof's headless engine are loaded (the first body on the page loads
 * them), a body of at most `markdownWorkerThreshold` characters renders synchronously in the
 * layout phase of the commit that mounts it (`useRenderedMarkdown`); only the very first render
 * on a page, or a schema that failed to load, takes the asynchronous path. A longer body is
 * parsed off the page's own thread, so it holds its own text, marked `data-markdown-pending`,
 * until the parse worker answers (`markdown-engine.ts`), and the `block` variant then fills the
 * element a piece at a time (`paintBlockInSlices`), so the 16,000 elements of an 8,000-item list
 * never reach the page in one task. A preview that must clamp its rendered line is
 * `MarkdownPreview`, which parses with the same engine.
 */
export function MarkdownBody({
  markdown,
  onRendered,
  softBreaks = "space",
  variant = "block",
}: {
  markdown: string;
  onRendered?: () => void;
  /** What a single newline inside a paragraph becomes: `"space"` for a document (hard-wrapped
   *  Markdown is one paragraph), `"line"` for text whose author meant its lines as lines, such
   *  as a model's streamed turn (`SoftBreaks`). */
  softBreaks?: SoftBreaks;
  variant?: "block" | "inline";
}): ReactNode {
  // One ref per tag, since a `<div>` and a `<span>` type their refs differently.
  const blockRoot = useRef<HTMLDivElement>(null);
  const inlineRoot = useRef<HTMLSpanElement>(null);
  const inline = variant === "inline";
  const { isFallback, isPending, portals } = useRenderedMarkdown(
    inline ? inlineRoot : blockRoot,
    markdown,
    inline ? paintInline : paintBlock,
    {
      pictures: variant,
      onRendered,
      paintInSlices: inline ? undefined : paintBlockInSlices,
      softBreaks,
    }
  );

  return inline ? (
    <span
      className={markdownClassName}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-pending={isPending || undefined}
      ref={inlineRoot}
    >
      {portals}
    </span>
  ) : (
    <div
      className={`${markdownClassName} max-w-none`}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-pending={isPending || undefined}
      ref={blockRoot}
    >
      {portals}
    </div>
  );
}
