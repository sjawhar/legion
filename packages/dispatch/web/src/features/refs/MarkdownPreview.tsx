import { type ReactNode, useCallback, useRef } from "react";

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

/**
 * Every textblock's inline content, marks intact, on one line: a heading's words, then the
 * paragraph's bold and code and links, then each list item, space-separated. (Not
 * `MarkdownBody`'s inline flattening, whose later blocks are plain text: a preview whose first
 * block is a heading would lose the formatting of everything after it.) A code block's text goes
 * inside a `<code>`, so it reads as code and `linkifyDispatchRefs` leaves it alone.
 */
const paintOneLine: PaintMarkdown = (element, parsed, serializer) => {
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
};

/** A letter, digit or combining mark: what the server's text-search parser reads as part of a
 *  word. An underscore or a hyphen is not (`foo_bar` and `CORE-1` are two words each to it). */
const WORD_CHARACTER = /[\p{L}\p{M}\p{N}]/u;

function isWordCharacter(character: string | undefined): boolean {
  return character !== undefined && WORD_CHARACTER.test(character);
}

/** One search hit, matched case-insensitively and only as a whole word where it has a word's
 *  edge: the server marks whole lexemes (`port`, never the `port` in `support`), and a hit that
 *  starts or ends with punctuation (`-1` of `CORE-1`) is bounded only on its word side. */
interface Phrase {
  readonly pattern: RegExp;
  readonly boundedStart: boolean;
  readonly boundedEnd: boolean;
}

function phrasesOf(highlight: readonly string[]): Phrase[] {
  return highlight.flatMap((raw) => {
    const phrase = raw.trim();
    if (phrase === "") {
      return [];
    }
    // `u` keeps every index on the source text itself; lower-casing first would not, since a
    // character such as `İ` lower-cases to two code units and every slice after it would drift.
    const escaped = phrase.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    return [
      {
        boundedEnd: isWordCharacter(Array.from(phrase).at(-1)),
        boundedStart: isWordCharacter(Array.from(phrase)[0]),
        pattern: new RegExp(escaped, "giu"),
      },
    ];
  });
}

/** Where the first whole-word occurrence of any phrase in `text` at or after `from` is: the
 *  earliest, and of two starting together the longer. `undefined` when none occurs. */
function nextPhrase(
  text: string,
  phrases: readonly Phrase[],
  from: number
): { at: number; length: number } | undefined {
  let best: { at: number; length: number } | undefined;
  for (const { boundedEnd, boundedStart, pattern } of phrases) {
    pattern.lastIndex = from;
    for (let match = pattern.exec(text); match !== null; match = pattern.exec(text)) {
      const at = match.index;
      const end = at + match[0].length;
      const startsWord = !boundedStart || !isWordCharacter(text[at - 1]);
      const endsWord = !boundedEnd || !isWordCharacter(text[end]);
      if (startsWord && endsWord) {
        if (
          best === undefined ||
          at < best.at ||
          (at === best.at && match[0].length > best.length)
        ) {
          best = { at, length: match[0].length };
        }
        break;
      }
      pattern.lastIndex = at + 1;
    }
  }
  return best;
}

function markElement(className: string): HTMLElement {
  const mark = document.createElement("mark");
  mark.className = className;
  return mark;
}

/**
 * Wraps every whole-word occurrence of each phrase in root's rendered text in a `<mark>`, for a
 * search hit: the server marks the words that matched in the snippet's source, the snippet then
 * renders as Markdown (which cannot carry the marks through), and this puts them back on the
 * rendered text. Each text node is searched on its own (the server marks one lexeme at a time,
 * so a hit never spans a formatting boundary), with the first-found longest phrase winning where
 * two overlap.
 */
function markPhrases(root: HTMLElement, phrases: readonly Phrase[], className: string): void {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const nodes: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text) {
      nodes.push(node);
    }
  }
  for (const node of nodes) {
    const text = node.data;
    const fragment = document.createDocumentFragment();
    let cursor = 0;
    for (let hit = nextPhrase(text, phrases, 0); hit !== undefined; ) {
      fragment.appendChild(document.createTextNode(text.slice(cursor, hit.at)));
      const mark = markElement(className);
      mark.textContent = text.slice(hit.at, hit.at + hit.length);
      fragment.appendChild(mark);
      cursor = hit.at + hit.length;
      hit = nextPhrase(text, phrases, cursor);
    }
    // A node with no hit is left as it is: `cursor` only moves past a mark.
    if (cursor > 0) {
      fragment.appendChild(document.createTextNode(text.slice(cursor)));
      node.replaceWith(fragment);
    }
  }
}

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
 * snippet, a hover card's body, a reply quote, a margin card's collapsed root. The text is
 * parsed with the same engine and schema as `MarkdownBody`, so bold, code, links and
 * `dispatch://` references render as they do in the full body, and block structure (a
 * heading, a list, a second paragraph) collapses onto one run of inline content as
 * `MarkdownBody`'s `inline` variant does. The clamp is CSS on the rendered element, after the
 * parse, so a cut never leaves half a mark: the reader sees `**held**` as bold "held" cut by
 * an ellipsis where it overflows, never a stray `**`. The source itself is never truncated
 * before the parse, which would do exactly that. A Dispatch picture shows as its thumbnail beside
 * its caption (`DispatchPicture`'s `inline`), as `MarkdownBody`'s `inline` variant shows it.
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
 * that needs a flex item passes `min-w-0` there as it would for any block. `highlight` names
 * phrases to wrap in a `<mark>` of `highlightClassName` once rendered (a search hit's matched
 * words; see `markPhrases`); a reference whose source holds one is marked whole, since its
 * words are replaced by its title. Until the engine has answered (one pre-paint commit once it
 * is loaded on the page) the element is empty; when the engine cannot load, the literal source
 * renders as text with `data-markdown-fallback`, as `MarkdownBody` does.
 */
export function MarkdownPreview({
  className = "",
  fullTitle = false,
  highlight,
  highlightClassName = "",
  lead,
  lines,
  links = "inert",
  markdown,
}: {
  className?: string;
  /** Whether a pointer reader sees the whole text on hover, for a surface whose text is long
   *  enough to be cut: its plain words, as `TruncatedText`'s `title` offers, never its source. */
  fullTitle?: boolean;
  highlight?: readonly string[];
  highlightClassName?: string;
  /** The app's own words before the text, plain and never parsed: a reply quote's
   *  `Replying to Planner — `. Clamped with the text as one line. */
  lead?: string;
  lines: 1 | 2 | 3;
  /** `inert` (the default) renders every link as link-styled text; `live` keeps them links, for
   *  a host whose own control is a toggle. */
  links?: "inert" | "live";
  markdown: string;
}): ReactNode {
  const root = useRef<HTMLSpanElement>(null);
  // Keyed on the phrases' joined text, so a caller handing a fresh array each render with the
  // same phrases does not re-parse the body. Postgres text holds no NUL, so none is in a phrase.
  const highlightKey = highlight?.join("\u0000");
  const decorate = useCallback<DecorateMarkdown>(
    (element, anchors) => {
      const replaced = links === "inert" ? inertLinks(element) : undefined;
      const phrases = highlightKey === undefined ? [] : phrasesOf(highlightKey.split("\u0000"));
      if (phrases.length > 0) {
        markPhrases(element, phrases, highlightClassName);
      }
      const decorated = anchors.map((reference) => {
        const host = replaced === undefined ? reference.anchor : replaced.get(reference.anchor);
        if (host === undefined) {
          throw new Error("A reference anchor is not a link of the preview it was found in.");
        }
        // The reference's words (`dispatch://CORE-1`, or a link's own text) are gone by now,
        // replaced by its title once that resolves; a hit the server found in them marks the
        // whole reference.
        if (phrases.length > 0 && nextPhrase(reference.text, phrases, 0) !== undefined) {
          const mark = markElement(highlightClassName);
          host.replaceWith(mark);
          mark.appendChild(host);
        }
        return { ...reference, anchor: host };
      });
      if (lead !== undefined && lead !== "") {
        element.prepend(document.createTextNode(lead));
      }
      return decorated;
    },
    [highlightClassName, highlightKey, lead, links]
  );
  const { isFallback, portals } = useRenderedMarkdown(root, markdown, paintOneLine, {
    pictures: "inline",
    decorate,
  });
  const title = useMarkdownHeadline(fullTitle ? markdown : undefined, Number.POSITIVE_INFINITY);

  return (
    <span
      className={`${CLAMP_CLASS[lines]} ${markdownClassName} max-w-none ${className}`}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-preview=""
      ref={root}
      title={title?.text}
    >
      {portals}
    </span>
  );
}
