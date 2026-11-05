import { type ReactNode, useLayoutEffect } from "react";

import { ArtifactMedia } from "./ArtifactMedia";
import { referenceSpans } from "./reference-spans";
import { usePinnedMedia, useReferencePath, useReferenceTarget } from "./reference-target";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  nodeRouteFromHref,
  parseDispatchReference,
  referenceRouteFromHref,
  shortForm,
} from "./routes";

export interface ReferenceAnchor {
  /** Whether the anchor is the only content of the paragraph it sits in: the form a plain
   *  reference takes when its author wrote it on a line of its own, and the only one where a
   *  reference to a video draws its player (`RefLink`). */
  readonly alone: boolean;
  /** The element the `RefLink` portal renders into: the `<a>` itself, or whatever a surface put
   *  in its place (`MarkdownPreview` keeps no link inside the link it sits in). */
  readonly anchor: HTMLElement;
  readonly key: string;
  readonly route: DispatchReferenceRoute;
  /** The words the anchor showed before they were cleared for the portal: a bare reference's
   *  `dispatch://…`, or the text a Markdown link gave it. */
  readonly text: string;
}

/** Portal content for an inline reference anchor: the resolved title once
 * `useReferenceTarget` has it, the ref's short form until then, and beside it the picture's
 * thumbnail when the reference names an image artifact. It shows no document text, so it reads
 * none. A thumbnail the browser cannot draw (a pinned version the server does not serve) is
 * dropped, leaving the title alone, so a stale or hand-written reference never shows a
 * broken-image glyph. A reference to a video standing alone in its block is its player instead,
 * drawn at reading size as a picture line's is, and the anchor stops being a link for as long
 * as the player is there, so a press on its controls plays rather than opening the artifact's
 * page (`ArtifactMedia`); inside a sentence it stays the link it has always been. When the
 * element it renders into is a link, its href follows `useReferencePath`, which a pinned block
 * of a spec resolves only once its issue's summary is read. */
export function RefLink({
  alone,
  anchor,
  route,
}: {
  alone?: boolean;
  anchor?: HTMLElement;
  route: DispatchReferenceRoute;
}): ReactNode {
  const { picture, title } = useReferenceTarget(route, { text: false });
  const path = useReferencePath(route);
  // What the reference draws: the summary's answer, which for a pinned reference carries the
  // address the reference itself names, and until it is read the pinned read's own answer,
  // which asks the cache and waits for nothing.
  const pinned = usePinnedMedia(route);
  const media = picture ?? pinned;
  const player = alone === true && media?.kind === "video" ? media.src : undefined;
  useLayoutEffect(() => {
    if (anchor instanceof HTMLAnchorElement && player === undefined) {
      anchor.setAttribute("href", path);
    }
  }, [anchor, path, player]);
  const label = title ?? shortForm(route);
  if (player !== undefined) {
    return (
      <ArtifactMedia
        caption={label}
        fallback={label}
        host={anchor}
        kind="video"
        src={player}
        variant="block"
      />
    );
  }
  // A video inside a sentence shows no thumbnail, as a target that is no picture shows none.
  const thumbnail = picture?.kind === "picture" ? picture.src : undefined;
  if (thumbnail === undefined) {
    return <>{label}</>;
  }
  return (
    <ArtifactMedia
      caption=""
      fallback={label}
      kind="picture"
      src={thumbnail}
      variant="thumbnail"
      words={label}
    />
  );
}

const excludedRefAncestorTags: Record<string, true> = { A: true, CODE: true, PRE: true };

function isInsideExcludedAncestor(node: Node, root: Node): boolean {
  for (
    let current = node.parentNode;
    current !== null && current !== root;
    current = current.parentNode
  ) {
    if (current instanceof HTMLElement && excludedRefAncestorTags[current.tagName] === true) {
      return true;
    }
  }
  return false;
}

/**
 * Wraps every bare `dispatch://…` reference in root's text into a real `<a>`, so
 * `collectReferenceAnchors` can then resolve it like any other link; a reference ends where the
 * composer's and the server's do (`referenceSpans`). Text already inside a link, inline code
 * span, or code block is left alone (its ancestor already parsed as a distinct node, so there is
 * no need to re-tokenize raw Markdown to find code/link boundaries by hand). Only `dispatch://`
 * needs this pass — remark-gfm already autolinks bare `http(s)://` URLs into real link marks
 * during Markdown parsing, before this ever runs.
 */
export function linkifyDispatchRefs(root: HTMLElement): void {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const candidates: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text && !isInsideExcludedAncestor(node, root)) {
      candidates.push(node);
    }
  }
  for (const textNode of candidates) {
    const value = textNode.data;
    if (!value.includes("dispatch://")) {
      continue;
    }
    const fragment = document.createDocumentFragment();
    let lastIndex = 0;
    let replaced = false;
    for (const { start, value: raw } of referenceSpans(value, "dispatch://")) {
      if (parseDispatchReference(raw) === undefined) {
        continue;
      }
      fragment.appendChild(document.createTextNode(value.slice(lastIndex, start)));
      const anchor = document.createElement("a");
      anchor.href = raw;
      anchor.textContent = raw;
      fragment.appendChild(anchor);
      lastIndex = start + raw.length;
      replaced = true;
    }
    if (!replaced) {
      continue;
    }
    fragment.appendChild(document.createTextNode(value.slice(lastIndex)));
    textNode.replaceWith(fragment);
  }
}

/**
 * Whether an anchor's target is a URL its author left as text: what GFM autolinks, a bare
 * `https://…` (a bare `www.…`, whose target gains `http://`) or one in angle brackets
 * (`<dispatch://…>` too), and the anchor `linkifyDispatchRefs` wraps around a bare
 * `dispatch://…`, all show their target as their words. A Markdown link whose words are its own
 * target (`[dispatch://X](dispatch://X)`, the form a document stores an autolink in) reads the
 * same, and is the same text to the server. `target` is the anchor's own target: Proof's
 * serializer moves a `dispatch://` target into `data-dispatch-href` and empties `href`.
 */
function isTextUrl(target: string, text: string): boolean {
  return target === text || target === `http://${text}`;
}

/**
 * Finds every already-rendered `<a>` in root whose href is a dispatch:// reference or a
 * same-origin dashboard path — a link `linkifyDispatchRefs` just inserted, an ordinary Markdown
 * link a user wrote by hand, or a bare `http(s)://` URL remark-gfm autolinked — rewrites its href
 * to the SPA route via `buildReferencePath`, and clears its text so the caller can
 * portal a `RefLink` in to render the resolved title. A URL its author left as text
 * (`isTextUrl`) is read as the server's `text.Extract` reads text (`referenceRouteFromHref`): one
 * whose block is past the bounds the index keys names its document, as the server cites it; an
 * `http(s)://` one past them stays the link it is; and a `dispatch://` one that names nothing
 * becomes its words as plain text, as a bare one already renders, with no link to click and no
 * hover trigger. A link its author wrote names its target explicitly, and drawing or opening it
 * is a read, so its href is read as the server reads a request (`nodeRouteFromHref`), whatever the
 * length of its names. An external link, or an author's href that fails to parse as a reference,
 * is left untouched, and so is a picture's link (`data-dispatch-picture`, the engine's
 * `pictureSerializer`), whose content is a `DispatchPicture` rather than a title.
 *
 * `@legion/proof-editor`'s Markdown link serializer sanitizes a `dispatch://` href to `""`
 * (Milkdown's link sanitizer only allows http/https/mailto/tel/ftp — a document strangers can
 * edit should never render an attacker-chosen non-http scheme as a clickable href), but tags the
 * anchor with `data-dispatch-href` carrying the original target. `linkifyDispatchRefs`'s own
 * freshly-wrapped anchors, and a same-origin dashboard URL a remark-gfm autolinked, still carry
 * their real value in `href` — `data-dispatch-href` takes priority when present.
 */
export function collectReferenceAnchors(
  root: HTMLElement,
  appOrigin: string = window.location.origin
): ReferenceAnchor[] {
  const targets: ReferenceAnchor[] = [];
  let index = 0;
  for (const anchor of root.querySelectorAll<HTMLAnchorElement>("a:not([data-dispatch-picture])")) {
    const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
    if (href === null) {
      continue;
    }
    const text = anchor.textContent ?? "";
    const textUrl = isTextUrl(href, text);
    const route = textUrl
      ? referenceRouteFromHref(href, appOrigin)
      : nodeRouteFromHref(href, appOrigin);
    if (route === undefined) {
      // A `dispatch://` URL left as text that cites nothing - past the bounds the index keys -
      // is the text it is, as a bare one past them already renders (`linkifyDispatchRefs`):
      // Proof's serializer left it `href=""` and `data-dispatch-href`, which would click to the
      // current page and open a hover card for a reference the server never recorded.
      if (textUrl && href.startsWith("dispatch://")) {
        anchor.replaceWith(document.createTextNode(text));
      }
      continue;
    }
    anchor.setAttribute("href", buildReferencePath(route));
    const reference = buildDispatchReference(route);
    anchor.setAttribute("data-dispatch-ref", reference);
    anchor.replaceChildren();
    targets.push({
      alone: isAloneInParagraph(anchor),
      anchor,
      key: `${reference}:${index}`,
      route,
      text,
    });
    index += 1;
  }
  return targets;
}

/** Whether the anchor is the whole of the paragraph it sits in: the reference its author wrote
 *  on a line of its own, which a video draws its player for. A paragraph is the block a plain
 *  reference stands alone in - a reference inside a sentence has words beside it, and one
 *  inside a heading, a table cell or a list item is in a line of other content - and whitespace
 *  around it is no content. What the anchor itself holds says nothing here, so clearing its
 *  words for the portal leaves the answer as it was. */
function isAloneInParagraph(anchor: HTMLAnchorElement): boolean {
  const block = anchor.parentElement;
  if (block === null || block.tagName !== "P") {
    return false;
  }
  return Array.from(block.childNodes).every(
    (node) =>
      node === anchor ||
      (node.nodeType === Node.TEXT_NODE && (node.textContent ?? "").trim() === "")
  );
}
