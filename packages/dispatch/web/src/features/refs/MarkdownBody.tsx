import type { Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useRef } from "react";

import { markdownClassName, type SoftBreaks } from "./markdown-engine";
import { paintBlock, paintBlockInSlices } from "./paint-block";
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
 * A picture whose address is a Dispatch artifact (`![shot.png](dispatch://…@v1)`, what a pasted
 * picture is written as, or the same without `@vN` for its latest version) shows through
 * `DispatchPicture`: at its own size scaled down to the column, opening at full size on a click,
 * in `block`; as a thumbnail beside its caption in `inline`. A picture on another website renders
 * as Proof renders it.
 *
 * Once the schema and Proof's headless engine are loaded (the first body on the page loads
 * them), a body of at most `markdownWorkerThreshold` characters renders synchronously in the
 * layout phase of the commit that mounts it (`useRenderedMarkdown`); only the very first render
 * on a page, or a schema that failed to load, takes the asynchronous path. A longer body is
 * parsed off the page's own thread, so it holds its own text, marked `data-markdown-pending`,
 * until the parse worker answers (`markdown-engine.ts`), and the `block` variant then fills the
 * element a piece at a time (`paintBlockInSlices`), so the 16,000 elements of an 8,000-item list
 * never reach the page in one task. A body is not a document, so raw HTML in one is read as the
 * text it shows (`rawHtml: "text"`): an HTML comment - the `<!-- legion: … -->` footer an agent
 * appends, say - draws as nothing, as GitHub draws it, and any other raw HTML draws as its own
 * literal characters, never as HTML, with the Markdown around it formatted as ever. A preview
 * that must clamp its rendered line is `MarkdownPreview`, which parses with the same engine and
 * the same policy.
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
      rawHtml: "text",
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
