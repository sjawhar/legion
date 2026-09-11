import type { HeadlessProofEditor } from "@sjawhar/proof-editor/headless";
import { DOMSerializer, type Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useEffect, useRef } from "react";

let headlessProof: Promise<HeadlessProofEditor> | undefined;

function loadHeadlessProof(): Promise<HeadlessProofEditor> {
  headlessProof ??= import("@sjawhar/proof-editor/headless").then(({ createHeadlessProof }) =>
    createHeadlessProof()
  );
  return headlessProof;
}

/** Proof's schema has no node for CommonMark's raw-HTML block/inline spans (a bare tag-shaped
 * substring outside a code span or fence, e.g. `<img src=x>`), so `parseMarkdown` throws for
 * it below. HTML-like text already inside a code span or fence parses safely as literal text
 * (the parser never treats code content as raw HTML), so nothing needs escaping there -
 * pre-escaping the whole source regardless of context, as an earlier version of this component
 * did, printed literal backslashes around any code span or fence containing something
 * tag-shaped. Falling back to the raw string as one plain-text node on a parse failure keeps
 * that one edge case inert without corrupting the (far more common) case of tag-shaped text
 * quoted in code. */

/** The first textblock's own inline content (marks intact), plus the flattened plain text of
 * every textblock after it, joined with a single space - see `MarkdownBody`'s `inline` variant. */
interface InlineContent {
  head: ProseMirrorNode;
  extra: string;
}

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
      const text = node.textContent.trim();
      if (text !== "") {
        extra.push(text);
      }
    }
    return false;
  });
  return head === undefined ? undefined : { extra: extra.join(" "), head };
}

const markdownClassName =
  "dispatch-markdown prose prose-sm prose-slate break-words dark:prose-invert";

/**
 * Renders Markdown text through Proof's own parser and schema, so every question, answer,
 * comment, reply, and message formats identically to the document editor. `block` (the
 * default) renders the full parsed document, including lists and headings, inside a `<div>`.
 * `inline` renders only inline marks (bold, code, links) inside a `<span>` with no wrapping
 * `<p>` or block spacing, for single-line contexts like an option label or a clamped preview:
 * it takes the first textblock's inline content verbatim (marks intact) and appends every
 * later textblock's plain text after it, space-joined, so a list or a multi-paragraph body
 * collapses onto one line instead of nesting a `<ul>`/second `<p>` inside the span. Both
 * variants inherit the surrounding text's size and color (`.dispatch-markdown` in styles.css
 * overrides Tailwind Typography's fixed palette, heading scale, and font size) so a heading
 * inside, say, an ask question reads as bold text at the card's own size rather than a
 * page-size h1, and a resolution reason inline in a `text-xs` line stays that size.
 */
export function MarkdownBody({
  markdown,
  onRendered,
  variant = "block",
}: {
  markdown: string;
  onRendered?: () => void;
  variant?: "block" | "inline";
}): ReactNode {
  const onRenderedRef = useRef(onRendered);
  onRenderedRef.current = onRendered;
  const blockRoot = useRef<HTMLDivElement>(null);
  const inlineRoot = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    let mounted = true;
    const root = variant === "inline" ? inlineRoot.current : blockRoot.current;
    void loadHeadlessProof().then((proof) => {
      if (!mounted) {
        return;
      }
      if (root === null) {
        throw new Error("MarkdownBody's root is unavailable.");
      }
      let parsed: ProseMirrorNode | undefined;
      try {
        parsed = proof.parseMarkdown(markdown);
      } catch {
        parsed = undefined;
      }
      const serializer = DOMSerializer.fromSchema(proof.schema);
      if (parsed === undefined) {
        root.replaceChildren(document.createTextNode(markdown));
      } else if (variant === "inline") {
        const flattened = flattenInline(parsed);
        root.replaceChildren();
        if (flattened !== undefined) {
          root.appendChild(serializer.serializeFragment(flattened.head.content));
          if (flattened.extra !== "") {
            root.appendChild(document.createTextNode(` ${flattened.extra}`));
          }
        }
      } else {
        root.replaceChildren(serializer.serializeFragment(parsed.content));
        for (const item of root.querySelectorAll("li[data-spread='false']")) {
          const paragraph = item.firstElementChild;
          if (item.childElementCount === 1 && paragraph?.tagName === "P") {
            paragraph.replaceWith(...paragraph.childNodes);
          }
        }
      }
      onRenderedRef.current?.();
    });
    return () => {
      mounted = false;
    };
  }, [markdown, variant]);

  return variant === "inline" ? (
    <span className={markdownClassName} ref={inlineRoot} />
  ) : (
    <div className={`${markdownClassName} max-w-none`} ref={blockRoot} />
  );
}
