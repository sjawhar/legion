import type { MouseEvent } from "react";
import type { NavigateFunction } from "react-router-dom";

import { buildReferencePath, parseDispatchReference } from "../refs/routes";

/** A rendered link's `dispatch://` target, if any. `@legion/proof-editor`'s Markdown
 * serializer sanitizes a `dispatch://` href to `""` (Milkdown's link sanitizer only allows
 * http/https/mailto/tel/ftp — a document strangers can edit should never render an
 * attacker-chosen non-http scheme as a clickable href) and tags the anchor with
 * `data-dispatch-href` carrying the original target instead; a linked picture's anchor carries the
 * same attributes, and a picture's fallback link carries its reference there beside its real SPA
 * path. */
function dispatchHrefOf(anchor: Element): string | null {
  const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
  return href?.startsWith("dispatch://") ? href : null;
}

/**
 * A document view's click on a `dispatch://` link - a link mark, a linked picture's anchor, a
 * picture's fallback link - navigated in the app, on a plain primary click. `ProofDocument` (the
 * live document) and `VersionView` (a past version, read-only) both call it from their article's
 * `onClickCapture`: the capture phase runs before the editor's own click handling, so the
 * navigation is the click's one effect and proof-editor's `markdownLinkClickPlugin` never also
 * opens a fallback link's page in a new tab in a read-only view.
 *
 * A modifier click (open in a new tab or window) is left to the browser and the editor. So is a
 * click ending a drag-selection that happens to end on a link - except on a linked picture: a
 * click on an atom picture makes ProseMirror's node selection, which is a non-collapsed DOM
 * selection.
 */
export function followDispatchLink(
  event: MouseEvent<HTMLElement>,
  navigate: NavigateFunction
): void {
  const modified = event.ctrlKey || event.metaKey || event.shiftKey || event.altKey;
  if (event.button !== 0 || modified) {
    return;
  }
  const target = event.target as Element;
  const anchor = target.closest<HTMLAnchorElement>("a");
  const href = anchor === null ? null : dispatchHrefOf(anchor);
  const route = href === null ? undefined : parseDispatchReference(href);
  if (anchor === null || route === undefined) {
    return;
  }
  const selection = window.getSelection();
  const picture = target.closest("img, [data-dispatch-doc-picture]");
  const onPicture = picture !== null && anchor.contains(picture);
  if (selection !== null && !selection.isCollapsed && !onPicture) {
    return;
  }
  event.preventDefault();
  navigate(buildReferencePath(route));
}
