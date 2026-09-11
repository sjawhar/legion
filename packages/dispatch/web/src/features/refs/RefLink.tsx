import type { ReactNode } from "react";

import { trimReference } from "../margin/Composer";
import {
  buildDispatchReference,
  buildIssuePath,
  buildProjectPath,
  type DispatchReferenceRoute,
  isProjectRoute,
  parseDispatchReference,
  parseIssuePath,
  parseProjectPath,
} from "./routes";
import { useReferenceTarget } from "./Unfurl";

export interface ReferenceAnchor {
  readonly anchor: HTMLAnchorElement;
  readonly key: string;
  readonly route: DispatchReferenceRoute;
}

/** The ref's compact fallback text, shown until `useReferenceTarget` resolves a title (or if
 * resolution never finds one, e.g. a deleted ask). Mirrors `buildDispatchReference`'s shape
 * without the `dispatch://` scheme, so it reads like a second, shorter reference. */
export function shortForm(route: DispatchReferenceRoute): string {
  if (isProjectRoute(route)) {
    const base = `${route.project}/${route.slug}`;
    return route.item === undefined ? base : `${base} ${route.item.kind}`;
  }
  switch (route.kind) {
    case "issue":
      return route.key;
    case "spec":
      return `${route.key} spec`;
    case "conversation":
      return `${route.key} log`;
    case "children":
      return `${route.key} children`;
    case "artifacts":
      return `${route.key} artifacts`;
    case "artifact":
      return `${route.key} ${route.slug}`;
    case "ask":
      return `${route.key} ask`;
    case "comment":
      return `${route.key} comment`;
    case "message":
      return `${route.key} message`;
  }
}

/** Portal content for an inline reference anchor: the resolved title once
 * `useReferenceTarget` has it, the ref's short form until then. */
export function RefLink({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { title } = useReferenceTarget(route);
  return <>{title ?? shortForm(route)}</>;
}

const bareDispatchRefPattern = /dispatch:\/\/\S+/g;

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
 * `collectReferenceAnchors` can then resolve it like any other link. Text already inside a link,
 * inline code span, or code block is left alone (its ancestor already parsed as a distinct node,
 * so there is no need to re-tokenize raw Markdown to find code/link boundaries by hand). Only
 * `dispatch://` needs this pass — remark-gfm already autolinks bare `http(s)://` URLs into real
 * link marks during Markdown parsing, before this ever runs.
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
    bareDispatchRefPattern.lastIndex = 0;
    if (!bareDispatchRefPattern.test(value)) {
      continue;
    }
    bareDispatchRefPattern.lastIndex = 0;
    const fragment = document.createDocumentFragment();
    let lastIndex = 0;
    let replaced = false;
    for (const match of value.matchAll(bareDispatchRefPattern)) {
      const start = match.index ?? 0;
      const raw = trimReference(match[0]);
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

function routeFromHref(href: string, appOrigin: string): DispatchReferenceRoute | undefined {
  if (href.startsWith("dispatch://")) {
    return parseDispatchReference(href);
  }
  let url: URL;
  try {
    url = new URL(href, appOrigin);
  } catch {
    return undefined;
  }
  if (url.origin !== appOrigin) {
    return undefined;
  }
  const route =
    parseIssuePath(url.pathname, url.search) ?? parseProjectPath(url.pathname, url.search);
  if (route === undefined || (isProjectRoute(route) && route.kind !== "document")) {
    return undefined;
  }
  return route;
}

/**
 * Finds every already-rendered `<a>` in root whose href is a dispatch:// reference or a
 * same-origin dashboard path — a link `linkifyDispatchRefs` just inserted, an ordinary Markdown
 * link a user wrote by hand, or a bare `http(s)://` URL remark-gfm autolinked — rewrites its href
 * to the SPA route via `buildIssuePath`/`buildProjectPath`, and clears its text so the caller can
 * portal a `RefLink` in to render the resolved title. An external link, or an href that fails to
 * parse as a reference, is left untouched.
 *
 * `@sjawhar/proof-editor`'s Markdown link serializer sanitizes a `dispatch://` href to `""`
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
    const route = routeFromHref(href, appOrigin);
    if (route === undefined) {
      continue;
    }
    anchor.setAttribute(
      "href",
      isProjectRoute(route) ? buildProjectPath(route) : buildIssuePath(route)
    );
    const reference = buildDispatchReference(route);
    anchor.setAttribute("data-dispatch-ref", reference);
    anchor.replaceChildren();
    targets.push({ anchor, key: `${reference}:${index}`, route });
    index += 1;
  }
  return targets;
}
