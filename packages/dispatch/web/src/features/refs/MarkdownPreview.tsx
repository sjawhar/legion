import type { Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useCallback, useRef } from "react";

import type { DisplayTree } from "../../api/types";
import { markdownClassName, useMarkdownHeadline } from "./markdown-engine";
import {
  type DecorateMarkdown,
  type PaintMarkdown,
  useRenderedMarkdown,
} from "./useRenderedMarkdown";

/** How many rendered lines a preview shows before the browser cuts it. Tailwind's `line-clamp-N`
 *  utilities are literal class names here so its scanner emits each rule. Each sets the
 *  `display` the clamp needs, so the root carries no `display` utility of its own: `.block` is
 *  emitted after them in the same layer and would win, and nothing would clamp. */
const CLAMP_CLASS: Record<1 | 2 | 3, string> = {
  1: "line-clamp-1",
  2: "line-clamp-2",
  3: "line-clamp-3",
};

/** A textblock's words where it holds nothing but text with no marks on it, which is what a
 *  preview can put in as plain words rather than as serialized inline content; `undefined` for
 *  one holding a mark, a hard break, a reference or a picture, which the serializer must write
 *  itself. */
function unmarkedText(node: ProseMirrorNode): string | undefined {
  let text = "";
  for (let child = 0; child < node.content.childCount; child += 1) {
    const inline = node.content.child(child);
    if (!inline.isText || inline.marks.length > 0) {
      return undefined;
    }
    text += inline.text ?? "";
  }
  return text;
}

/** Every run the server marked as matching a search, as the engine's serializer draws one
 *  (`markdown-engine.ts`, `displaySchemaOf`). The server reads the snippet and marks the words
 *  it matched, so nothing on this page looks for them in the rendered text. */
const HIGHLIGHT_SELECTOR = "mark[data-search-highlight]";

/**
 * Every textblock's inline content, marks intact, on one line: a heading's words, then the
 * paragraph's bold and code and links, then each list item, space-separated. (Not
 * `MarkdownBody`'s inline flattening, whose later blocks are plain text: a preview whose first
 * block is a heading would lose the formatting of everything after it.) A code block's text goes
 * inside a `<code>`, so it reads as code and `linkifyDispatchRefs` leaves it alone.
 *
 * Blocks holding nothing but unmarked words go in as one text node, however many they are: a
 * preview is one run of text, and a body of thousands of plain blocks - the 8,000 items of a
 * list - would otherwise put 16,000 nodes into the page for the three lines a reader sees, which
 * is ~2 s of WebKit's thread to style and lay out (sjawhar/legion#2519). What the preview reads
 * is the same either way, character for character.
 */
const paintOneLine: PaintMarkdown = (element, parsed, serializer) => {
  element.replaceChildren();
  let first = true;
  /** The plain words waiting to go in, so a run of them lands as one node. */
  let plain = "";
  const putThePlainWordsIn = (): void => {
    if (plain !== "") {
      element.appendChild(document.createTextNode(plain));
      plain = "";
    }
  };
  parsed.descendants((node) => {
    if (!node.isTextblock) {
      return true;
    }
    if (node.content.size === 0) {
      return false;
    }
    if (!first) {
      plain += " ";
    }
    first = false;
    const isCode = node.type.spec.code === true;
    const words = isCode ? undefined : unmarkedText(node);
    if (words !== undefined) {
      plain += words;
      return false;
    }
    putThePlainWordsIn();
    const content = serializer.serializeFragment(node.content);
    if (isCode) {
      const code = document.createElement("code");
      code.appendChild(content);
      element.appendChild(code);
    } else {
      element.appendChild(content);
    }
    return false;
  });
  putThePlainWordsIn();
  // A hard break inside a paragraph would start a second visual line inside a one-line
  // preview; the preview is one run of text, so it reads as a space.
  for (const br of element.querySelectorAll("br")) {
    br.replaceWith(document.createTextNode(" "));
  }
  // A hit the server marked inside a link's own words marks the whole link instead: a
  // reference's words are replaced by its title a moment later (`collectReferenceAnchors`
  // empties the anchor), so a mark left inside would go with them. A bare `dispatch://`
  // reference the server marked is inside its mark already, since the link is made from text
  // the mark holds (`linkifyDispatchRefs`).
  for (const link of element.querySelectorAll("a")) {
    if (link.querySelector(HIGHLIGHT_SELECTOR) === null) {
      continue;
    }
    for (const inner of link.querySelectorAll(HIGHLIGHT_SELECTOR)) {
      inner.replaceWith(...inner.childNodes);
    }
    const whole = document.createElement("mark");
    whole.setAttribute("data-search-highlight", "");
    link.replaceWith(whole);
    whole.appendChild(link);
  }
};

/**
 * Every link in the preview as inert, link-styled text. Each surface a preview sits in is
 * already one control - a link (a reply quote, an unfurl card, a hover card, a Broadcasts row),
 * a search option or a margin card's toggle - so a link inside it is a link in a link: a click
 * on it would follow the inner link and the outer control at once, and it would be a second tab
 * stop inside a tooltip. The words, their formatting and a reference's `data-dispatch-ref` (its
 * hover card) stay; the host is the one thing a click or a key press follows. Answers each
 * replaced link's span, so a reference's portal can follow its words into it.
 */
function inertLinks(root: HTMLElement): Map<HTMLElement, HTMLElement> {
  const replaced = new Map<HTMLElement, HTMLElement>();
  for (const link of root.querySelectorAll("a")) {
    const span = document.createElement("span");
    for (const { name, value } of link.attributes) {
      if (name !== "href" && name !== "rel" && name !== "target") {
        span.setAttribute(name, value);
      }
    }
    span.setAttribute("data-markdown-link", "");
    span.append(...link.childNodes);
    link.replaceWith(span);
    replaced.set(link, span);
  }
  return replaced;
}

/**
 * A line or two of Markdown someone wrote, formatted and then cut to fit: a search hit's
 * snippet, a hover card's body, a reply quote, a margin card's collapsed root. A stored text is
 * drawn from `tree`, the reading Go made of it, with the same engine and schema as
 * `MarkdownBody`, so bold, code, links and `dispatch://` references render as they do in the
 * full body, and block structure (a heading, a list, a second paragraph) collapses onto one run
 * of inline content as `MarkdownBody`'s `inline` variant does. The clamp is CSS on the rendered
 * element, after the drawing, so a cut never leaves half a mark: the reader sees `**held**` as
 * bold "held" cut by an ellipsis where it overflows, never a stray `**`. A caller that shows
 * only the start of a text cuts the tree, never the markdown (`tree-fragment.ts`), so no cut
 * can leave a dangling delimiter for anything to balance. A Dispatch picture shows as its
 * thumbnail beside its caption (`DispatchPicture`'s `inline`), as `MarkdownBody`'s `inline`
 * variant shows it.
 *
 * Every surface a preview sits in is one control already, so its links and references read as
 * links but are not ones (`inertLinks`): the host is what a click follows. A host whose own
 * control is a toggle rather than a navigation (the margin's collapsed card, which opens the
 * thread) passes `links="live"` and keeps them real: there a click on a reference is the one
 * way out of the card to its target, as it was before the preview, and its handler skips a
 * click under an `a`.
 *
 * `lines` is the clamp (`1` is `truncate`-like but keeps the inline markup; `2` and `3` wrap
 * then clamp). `className` styles the element, whose `display` is the clamp's own, so a parent
 * that needs a flex item passes `min-w-0` there as it would for any block.
 * `highlightClassName` styles each run the server marked as matching a search, which rides in
 * the snippet's own tree (`HIGHLIGHT_SELECTOR`); a reference whose words hold one is marked
 * whole, since those words are replaced by its title. Until the engine has answered (one
 * pre-paint commit once it is loaded on the page) the element is empty; a text with no tree
 * that is too long for the page's own thread holds its own source, clamped, marked
 * `data-markdown-pending`, until the parse worker answers (`markdown-engine.ts`); and when the
 * engine cannot load, or the tree does not fit its schema, the literal source renders as text
 * with `data-markdown-fallback`, as `MarkdownBody` does.
 */
export function MarkdownPreview({
  className = "",
  fullTitle = false,
  highlightClassName = "",
  lead,
  lines,
  links = "inert",
  markdown,
  tree,
}: {
  className?: string;
  /** Whether a pointer reader sees the whole text on hover, for a surface whose text is long
   *  enough to be cut: its plain words, as `TruncatedText`'s `title` offers, never its source. */
  fullTitle?: boolean;
  highlightClassName?: string;
  /** The app's own words before the text, plain and never parsed: a reply quote's
   *  `Replying to Planner — `. Clamped with the text as one line. */
  lead?: string;
  lines: 1 | 2 | 3;
  /** `inert` (the default) renders every link as link-styled text; `live` keeps them links, for
   *  a host whose own control is a toggle. */
  links?: "inert" | "live";
  markdown: string;
  /** The reading Go made of this stored text, which the preview draws; absent only for a text
   *  Dispatch does not store (`MarkdownBody`). */
  tree?: DisplayTree;
}): ReactNode {
  const root = useRef<HTMLSpanElement>(null);
  const decorate = useCallback<DecorateMarkdown>(
    (element, anchors) => {
      const replaced = links === "inert" ? inertLinks(element) : undefined;
      if (highlightClassName !== "") {
        for (const mark of element.querySelectorAll<HTMLElement>(HIGHLIGHT_SELECTOR)) {
          mark.className = highlightClassName;
        }
      }
      const decorated = anchors.map((reference) => {
        const host = replaced === undefined ? reference.anchor : replaced.get(reference.anchor);
        if (host === undefined) {
          throw new Error("A reference anchor is not a link of the preview it was found in.");
        }
        return { ...reference, anchor: host };
      });
      if (lead !== undefined && lead !== "") {
        element.prepend(document.createTextNode(lead));
      }
      return decorated;
    },
    [highlightClassName, lead, links]
  );
  const { isFallback, isPending, portals } = useRenderedMarkdown(
    root,
    markdown,
    tree,
    paintOneLine,
    { pictures: "inline", decorate }
  );
  // A text with no tree is one Dispatch does not store, and its own source is all its hover
  // text can come from (`useMarkdownHeadline`).
  const title = useMarkdownHeadline(
    fullTitle ? tree : undefined,
    Number.POSITIVE_INFINITY,
    fullTitle && tree === undefined ? markdown : undefined
  );

  return (
    <span
      className={`${CLAMP_CLASS[lines]} ${markdownClassName} max-w-none ${className}`}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-pending={isPending || undefined}
      data-markdown-preview=""
      ref={root}
      title={title?.text}
    >
      {portals}
    </span>
  );
}
