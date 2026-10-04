import type { HeadlessProofEditor } from "@legion/proof-editor/headless";
import { DOMSerializer } from "prosemirror-model";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import {
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

const NO_ANCHORS: readonly ReferenceAnchor[] = [];

/** How many rendered lines a preview shows before the browser cuts it. Tailwind's `line-clamp-N`
 *  utilities are literal class names here so its scanner emits each rule. */
const CLAMP_CLASS: Record<1 | 2 | 3, string> = {
  1: "line-clamp-1",
  2: "line-clamp-2",
  3: "line-clamp-3",
};

/**
 * Wraps every occurrence of each `phrase` in root's rendered text in a `<mark>`, for a search
 * hit: the server marks the words that matched in the snippet's source, the snippet then
 * renders as Markdown (which cannot carry the marks through), and this puts them back on the
 * rendered text. Each text node is searched on its own (a phrase never spans two nodes, which
 * the server's whole-word highlights do not either), case-insensitively, with the first-found
 * longest phrase winning where two overlap. Text already inside a link keeps its link; a
 * `<mark>` inside an `<a>` is fine.
 */
function markPhrases(root: HTMLElement, phrases: readonly string[], className: string): void {
  const wanted = phrases.map((phrase) => phrase.toLowerCase()).filter((phrase) => phrase !== "");
  if (wanted.length === 0) {
    return;
  }
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const nodes: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text) {
      nodes.push(node);
    }
  }
  for (const node of nodes) {
    const text = node.data;
    const lower = text.toLowerCase();
    const fragment = document.createDocumentFragment();
    let cursor = 0;
    let replaced = false;
    while (cursor < text.length) {
      let at = -1;
      let length = 0;
      for (const phrase of wanted) {
        const found = lower.indexOf(phrase, cursor);
        if (found !== -1 && (at === -1 || found < at || (found === at && phrase.length > length))) {
          at = found;
          length = phrase.length;
        }
      }
      if (at === -1) {
        break;
      }
      fragment.appendChild(document.createTextNode(text.slice(cursor, at)));
      const mark = document.createElement("mark");
      mark.className = className;
      mark.textContent = text.slice(at, at + length);
      fragment.appendChild(mark);
      cursor = at + length;
      replaced = true;
    }
    if (replaced) {
      fragment.appendChild(document.createTextNode(text.slice(cursor)));
      node.replaceWith(fragment);
    }
  }
}

/**
 * A line or two of Markdown someone wrote, formatted and then cut to fit: a search hit's
 * snippet, a hover card's body, a reply quote, a margin card's collapsed root. The text is
 * parsed with the same engine and schema as `MarkdownBody`, so bold, code, links and
 * `dispatch://` references render as they do in the full body, and block structure (a
 * heading, a list, a second paragraph) collapses onto one run of inline content as
 * `MarkdownBody`'s `inline` variant does. The clamp is CSS on the rendered element, after the
 * parse, so a cut never leaves half a mark: the reader sees `**held**` as bold "held" cut by
 * an ellipsis where it overflows, never a stray `**`. The source itself is never truncated
 * before the parse, which would do exactly that.
 *
 * `lines` is the clamp (`1` is `truncate`-like but keeps the inline markup; `2` and `3` wrap
 * then clamp). `className` styles the block it renders as, which is `display: -webkit-box`
 * once clamped, so a parent that needs a flex item passes `min-w-0` there as it would for any
 * block. `highlight` names phrases to wrap in a `<mark>` of `highlightClassName` once rendered
 * (a search hit's matched words; see `markPhrases`). `onRendered` fires after each parse lands,
 * for a caller that measures the result. Until the engine has answered (one pre-paint commit
 * once it is loaded on the page) the element is empty; when the engine cannot load, the
 * literal source renders as text with `data-markdown-fallback`, as `MarkdownBody` does.
 */
export function MarkdownPreview({
  className = "",
  highlight,
  highlightClassName = "",
  lead,
  lines,
  markdown,
  onRendered,
  title,
}: {
  className?: string;
  highlight?: readonly string[];
  highlightClassName?: string;
  /** The app's own words before the text, plain and never parsed: a reply quote's
   *  `Replying to Planner — `. Clamped with the text as one line. */
  lead?: string;
  lines: 1 | 2 | 3;
  markdown: string;
  onRendered?: () => void;
  /** What a pointer reader sees on hover, for a surface whose text is long enough to be cut:
   *  the full source, as `TruncatedText` offers. */
  title?: string;
}): ReactNode {
  const onRenderedRef = useRef(onRendered);
  onRenderedRef.current = onRendered;
  const root = useRef<HTMLSpanElement>(null);
  const [referenceAnchors, setReferenceAnchors] = useState<readonly ReferenceAnchor[]>(NO_ANCHORS);
  const [isFallback, setIsFallback] = useState(false);
  // The effect keys on the phrases' joined text, so a caller handing a fresh array each render
  // with the same phrases does not re-parse the body.
  const highlightKey = highlight?.join("\u0000");

  useLayoutEffect(() => {
    const element = root.current;
    const render = (proof: HeadlessProofEditor | undefined) => {
      if (element === null) {
        throw new Error("MarkdownPreview's root is unavailable.");
      }
      const parsed = proof === undefined ? undefined : parseMarkdownOrUndefined(proof, markdown);
      if (proof === undefined || parsed === undefined) {
        setIsFallback(true);
        element.replaceChildren(document.createTextNode(markdown));
      } else {
        // Every textblock's inline content, marks intact, on one line: a heading's words, then
        // the paragraph's bold and code and links, then each list item, space-separated. (Not
        // `flattenInline`, whose later blocks are plain text: a preview whose first block is a
        // heading would lose the formatting of everything after it.) A code block's text goes
        // inside a `<code>`, so it reads as code and `linkifyDispatchRefs` leaves it alone.
        const serializer = DOMSerializer.fromSchema(proof.schema);
        element.replaceChildren();
        let first = true;
        parsed.descendants((node) => {
          if (!node.isTextblock) {
            return true;
          }
          if (node.content.size === 0) {
            return false;
          }
          if (!first) {
            element.appendChild(document.createTextNode(" "));
          }
          first = false;
          const content = serializer.serializeFragment(node.content);
          if (node.type.spec.code === true) {
            const code = document.createElement("code");
            code.appendChild(content);
            element.appendChild(code);
          } else {
            element.appendChild(content);
          }
          return false;
        });
        // A hard break inside a paragraph would start a second visual line inside a one-line
        // preview; the preview is one run of text, so it reads as a space.
        for (const br of element.querySelectorAll("br")) {
          br.replaceWith(document.createTextNode(" "));
        }
      }
      linkifyDispatchRefs(element);
      const anchors = collectReferenceAnchors(element);
      if (highlightKey !== undefined) {
        markPhrases(element, highlightKey.split("\u0000"), highlightClassName);
      }
      if (lead !== undefined && lead !== "") {
        element.prepend(document.createTextNode(lead));
      }
      setReferenceAnchors(anchors.length === 0 ? NO_ANCHORS : anchors);
      onRenderedRef.current?.();
    };
    setIsFallback(false);
    return renderWithHeadlessProof(render);
  }, [highlightClassName, highlightKey, lead, markdown]);

  return (
    <span
      className={`block ${CLAMP_CLASS[lines]} ${markdownClassName} ${className}`}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-preview=""
      ref={root}
      title={title}
    >
      {referenceAnchors.map(({ anchor, key, route }) =>
        createPortal(<RefLink route={route} />, anchor, key)
      )}
    </span>
  );
}
