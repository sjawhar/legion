import type { Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useRef } from "react";

import { markdownClassName, type SoftBreaks } from "./markdown-engine";
import { drawTaskCheckboxes } from "./task-checkboxes";
import { type PaintMarkdown, useRenderedMarkdown } from "./useRenderedMarkdown";

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

/** The `view` block's frame slot, which the mounted block owns (`renderViewBlockShell`). */
const FRAME_SLOT = "[data-dispatch-view-frame]";

/** Whether this node is a frame slot or holds one, and so must stay in the document. */
function holdsFrameSlot(node: Node): node is Element {
  return (
    node instanceof Element && (node.matches(FRAME_SLOT) || node.querySelector(FRAME_SLOT) !== null)
  );
}

/** The `page` of every frame slot at or under `element`, in document order: what a repaint pairs
 *  a live element with a fresh one by. A block's own `#id` is only what its author wrote and can
 *  repeat, so it names nothing here. */
function slotPages(element: Element): string {
  const pages = element.matches(FRAME_SLOT)
    ? [element.getAttribute("data-dispatch-view-page") ?? ""]
    : [];
  for (const slot of element.querySelectorAll(FRAME_SLOT)) {
    pages.push(slot.getAttribute("data-dispatch-view-page") ?? "");
  }
  return pages.join(" ");
}

/** Writes the fresh element's attributes onto the live one it replaces. A frame slot keeps every
 *  attribute it already has, because the mounted block stamps its own; every other element is
 *  made to carry exactly what the fresh paint says. */
function copyAttributes(live: Element, fresh: Element): void {
  for (const attribute of [...fresh.attributes]) {
    if (live.getAttribute(attribute.name) !== attribute.value) {
      live.setAttribute(attribute.name, attribute.value);
    }
  }
  if (live.matches(FRAME_SLOT)) {
    return;
  }
  for (const name of live.getAttributeNames()) {
    if (!fresh.hasAttribute(name)) {
      live.removeAttribute(name);
    }
  }
}

/**
 * Writes the freshly painted children over the live ones, keeping every element that holds a
 * `view` block's frame slot where it is. An `<iframe>` that leaves the document, even for an
 * instant, loads again, and a second load reads as "the page moved away", which is permanent; so
 * a repaint must move no frame and must take none out.
 *
 * At each level the children that hold a slot are paired in order, by tag name and by the
 * sequence of `page` values under them. On a pair, the fresh attributes are written onto the
 * live element and its own children are spliced the same way, and a kept element is never passed
 * to `insertBefore`. Every other fresh child is inserted around that kept chain. Any mismatch
 * falls back to replacing the level's children, whose worst case is one reload.
 */
function spliceChildren(parent: Element, incoming: readonly Node[]): void {
  const liveKept = [...parent.childNodes].filter(holdsFrameSlot);
  const freshKept = incoming.filter(holdsFrameSlot);
  const paired =
    liveKept.length === freshKept.length &&
    liveKept.every((live, index) => {
      const fresh = freshKept[index];
      return (
        fresh !== undefined &&
        live.tagName === fresh.tagName &&
        slotPages(live) === slotPages(fresh)
      );
    });
  if (!paired) {
    parent.replaceChildren(...incoming);
    return;
  }
  const kept = new Set<Node>(liveKept);
  for (const child of [...parent.childNodes]) {
    if (!kept.has(child)) {
      child.remove();
    }
  }
  let keptIndex = 0;
  for (const node of incoming) {
    if (!holdsFrameSlot(node)) {
      parent.insertBefore(node, liveKept[keptIndex] ?? null);
      continue;
    }
    const live = liveKept[keptIndex];
    keptIndex += 1;
    if (live === undefined) {
      continue;
    }
    copyAttributes(live, node);
    // The slot itself is React's: no pass of the repaint writes inside it.
    if (!live.matches(FRAME_SLOT)) {
      spliceChildren(live, [...node.childNodes]);
    }
  }
}

const paintBlock: PaintMarkdown = (element, parsed, serializer) => {
  const painted = serializer.serializeFragment(parsed.content);
  if (element.querySelector(FRAME_SLOT) === null) {
    element.replaceChildren(painted);
  } else {
    spliceChildren(element, [...painted.childNodes]);
  }
  drawTaskCheckboxes(element);
  for (const item of element.querySelectorAll("li[data-spread='false']")) {
    const paragraph = item.firstElementChild;
    if (item.childElementCount === 1 && paragraph?.tagName === "P") {
      paragraph.replaceWith(...paragraph.childNodes);
    }
  }
};

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
 * them), a body renders synchronously in the layout phase of the commit that mounts it
 * (`useRenderedMarkdown`); only the very first render on a page, or a schema that failed to
 * load, takes the asynchronous path. A preview that must clamp its rendered line is
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
  const { isFallback, portals } = useRenderedMarkdown(
    inline ? inlineRoot : blockRoot,
    markdown,
    inline ? paintInline : paintBlock,
    { pictures: variant, onRendered, softBreaks }
  );

  return inline ? (
    <span
      className={markdownClassName}
      data-markdown-fallback={isFallback || undefined}
      ref={inlineRoot}
    >
      {portals}
    </span>
  ) : (
    <div
      className={`${markdownClassName} max-w-none`}
      data-markdown-fallback={isFallback || undefined}
      ref={blockRoot}
    >
      {portals}
    </div>
  );
}
