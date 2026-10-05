import type { ReactNode } from "react";

import { useReferenceTarget } from "./reference-target";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  parseDispatchReference,
  referenceRouteFromHref,
  referenceSpans,
  shortForm,
} from "./routes";

export interface ReferenceAnchor {
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
 * `useReferenceTarget` has it, the ref's short form until then. */
export function RefLink({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { title } = useReferenceTarget(route);
  return <>{title ?? shortForm(route)}</>;
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
 * Finds every already-rendered `<a>` in root whose href is a dispatch:// reference or a
 * same-origin dashboard path — a link `linkifyDispatchRefs` just inserted, an ordinary Markdown
 * link a user wrote by hand, or a bare `http(s)://` URL remark-gfm autolinked — rewrites its href
 * to the SPA route via `buildReferencePath`, and clears its text so the caller can
 * portal a `RefLink` in to render the resolved title. An external link, or an href that fails to
 * parse as a reference, is left untouched.
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
  for (const anchor of root.querySelectorAll("a")) {
    const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
    if (href === null) {
      continue;
    }
    const route = referenceRouteFromHref(href, appOrigin);
    if (route === undefined) {
      continue;
    }
    anchor.setAttribute("href", buildReferencePath(route));
    const reference = buildDispatchReference(route);
    anchor.setAttribute("data-dispatch-ref", reference);
    const text = anchor.textContent ?? "";
    anchor.replaceChildren();
    targets.push({ anchor, key: `${reference}:${index}`, route, text });
    index += 1;
  }
  return targets;
}
