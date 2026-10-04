import type { HeadlessProofEditor } from "@legion/proof-editor/headless";
import { DOMSerializer } from "prosemirror-model";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import {
  flattenInline,
  markdownClassName,
  parseMarkdownOrUndefined,
  renderWithHeadlessProof,
} from "./markdown-engine";
import {
  collectReferenceAnchors,
  linkifyDispatchRefs,
  type ReferenceAnchor,
  RefLink,
} from "./RefLink";

/** Loads the schema and headless Proof chunk before Markdown-bearing UI needs to render -
 *  re-exported here because `DeploymentResilience` warms the renderer by this module's name. */
export { warmMarkdownRenderer } from "./markdown-engine";

/** The anchor list of a body with no references: one shared value, so re-rendering such a body
 *  leaves the state untouched instead of committing a fresh empty array each time. */
const NO_ANCHORS: readonly ReferenceAnchor[] = [];

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
 *
 * Once the schema and Proof's headless engine are loaded (the first body on the page loads
 * them), a body renders synchronously in the layout phase of the commit that mounts it
 * (`renderWithHeadlessProof`); only the very first render on a page, or a schema that failed to
 * load, takes the asynchronous path. A preview that must clamp its rendered line is
 * `MarkdownPreview`, which parses with the same engine.
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
  const [referenceAnchors, setReferenceAnchors] = useState<readonly ReferenceAnchor[]>(NO_ANCHORS);
  const [isFallback, setIsFallback] = useState(false);

  useLayoutEffect(() => {
    const root = variant === "inline" ? inlineRoot.current : blockRoot.current;
    const render = (proof: HeadlessProofEditor | undefined) => {
      if (root === null) {
        throw new Error("MarkdownBody's root is unavailable.");
      }
      const parsed = proof === undefined ? undefined : parseMarkdownOrUndefined(proof, markdown);
      if (proof === undefined || parsed === undefined) {
        setIsFallback(true);
        root.replaceChildren(document.createTextNode(markdown));
      } else {
        const serializer = DOMSerializer.fromSchema(proof.schema);
        if (variant === "inline") {
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
      }
      // A dispatch:// reference stays literal text to Markdown (it isn't a scheme GFM
      // autolinks), so bare refs get wrapped into real links first; a same-origin dashboard
      // URL is already a real link by then (GFM autolinked it, or the author wrote it as
      // Markdown link syntax). Either way, every reference anchor gets its href rewritten to
      // the SPA route and its text handed to a portal-mounted `RefLink` for the resolved title.
      linkifyDispatchRefs(root);
      const anchors = collectReferenceAnchors(root);
      setReferenceAnchors(anchors.length === 0 ? NO_ANCHORS : anchors);
      onRenderedRef.current?.();
    };
    setIsFallback(false);
    return renderWithHeadlessProof(render);
  }, [markdown, variant]);

  const portals = referenceAnchors.map(({ anchor, key, route }) =>
    createPortal(<RefLink route={route} />, anchor, key)
  );

  return variant === "inline" ? (
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
