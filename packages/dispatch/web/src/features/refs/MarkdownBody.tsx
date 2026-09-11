import type { HeadlessProofEditor } from "@sjawhar/proof-editor/headless";
import { DOMSerializer, type Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import {
  collectReferenceAnchors,
  linkifyDispatchRefs,
  type ReferenceAnchor,
  RefLink,
} from "./RefLink";

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

/** A textblock's own text, skipping any code span or code block content — used only for the
 * "extra" (non-first) textblocks in the `inline` variant below, which flatten to plain text and
 * so would otherwise re-expose a code span's `dispatch://` ref as linkifiable bare text once its
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
  const [referenceAnchors, setReferenceAnchors] = useState<readonly ReferenceAnchor[]>([]);

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
      // A dispatch:// reference stays literal text to Markdown (it isn't a scheme GFM
      // autolinks), so bare refs get wrapped into real links first; a same-origin dashboard
      // URL is already a real link by then (GFM autolinked it, or the author wrote it as
      // Markdown link syntax). Either way, every reference anchor gets its href rewritten to
      // the SPA route and its text handed to a portal-mounted `RefLink` for the resolved title.
      linkifyDispatchRefs(root);
      setReferenceAnchors(collectReferenceAnchors(root));
      onRenderedRef.current?.();
    });
    return () => {
      mounted = false;
    };
  }, [markdown, variant]);

  const portals = referenceAnchors.map(({ anchor, key, route }) =>
    createPortal(<RefLink route={route} />, anchor, key)
  );

  return variant === "inline" ? (
    <span className={markdownClassName} ref={inlineRoot}>
      {portals}
    </span>
  ) : (
    <div className={`${markdownClassName} max-w-none`} ref={blockRoot}>
      {portals}
    </div>
  );
}
