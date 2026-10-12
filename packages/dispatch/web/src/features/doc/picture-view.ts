import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { NodeView } from "@milkdown/kit/prose/view";

import { artifactVersionPath } from "../../api/client";
import { pictureClassName } from "../refs/DispatchPicture";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  pictureReference,
  shortForm,
} from "../refs/routes";

/** The link a picture falls back to: the reference's short form, with the real SPA path as its
 * `href` (a modifier click, a middle click and the read-only version view follow it) and
 * `data-dispatch-href` for the live editor's click handler and the hover card. The words are the
 * reference's own, never an author's caption, as `RefLink` shows them until a title resolves. */
function fallbackLink(
  route: DispatchReferenceRoute,
  document: Document,
  placeCaretAfter: () => void
): HTMLAnchorElement {
  const link = document.createElement("a");
  link.setAttribute("href", buildReferencePath(route));
  link.dataset.dispatchHref = buildDispatchReference(route);
  link.textContent = shortForm(route);
  // Not a drag source: the wrapper (ProseMirror's draggable leaf root) is, so a drag that starts
  // on the link moves the picture node as one on the `<img>` does, instead of dropping the
  // browser's own link HTML into the document.
  link.draggable = false;
  // Firefox does not focus a link on mousedown: the editing host takes the focus, and ProseMirror
  // then writes the editor's own selection into the DOM - the document's start, for a reader who
  // has not clicked into the text - which scrolls a long document away from the link before the
  // mouseup and the click land on it. Putting the caret beside the picture first makes that the
  // selection ProseMirror restores, so nothing scrolls. Chromium focuses the link itself, and an
  // unfocused editor moves its caret without touching the DOM selection. Not `preventDefault`:
  // Firefox would then start no drag from the link.
  link.addEventListener("mousedown", (event) => {
    if (event.button === 0) placeCaretAfter();
  });
  return link;
}

/**
 * The node view of Milkdown's `image` node. The browser cannot fetch `dispatch://`, so a picture
 * whose source names an artifact at a version (`![shot.png](dispatch://KEY/artifact/shot-png@v1)`,
 * the line a pasted picture is written as) is drawn from the version's same-origin bytes route,
 * with the caption `pictureReference` reads (`refs/routes.ts`, the rule it shares with
 * `pictureSerializer`): the `alt`, or the artifact's slug when that is empty. A version the
 * browser cannot draw (a document, another file, a missing version) errors, and the picture
 * becomes `fallbackLink`; a source that pins no version or names something other than an artifact
 * is that link at once. `placeCaretAfter` puts the editor's caret just after the node, which the
 * link calls on a primary-button mousedown (the comment in `fallbackLink` says why). Any other
 * source - a picture on another website, a relative path, a `dispatch://` string the reference
 * grammar refuses - is drawn by the schema's own `toDOM`, as today.
 */
export function pictureView(
  node: ProseMirrorNode,
  document: Document,
  placeCaretAfter: () => void
): NodeView {
  const picture = pictureReference(node.attrs);
  if (picture === undefined) {
    const toDOM = node.type.spec.toDOM;
    if (toDOM === undefined) {
      throw new Error(`the ${node.type.name} node has no toDOM`);
    }
    return { dom: DOMSerializer.renderSpec(document, toDOM(node)).dom };
  }
  const { named, route } = picture;
  const dom = document.createElement("span");
  dom.dataset.dispatchDocPicture = buildDispatchReference(route);
  let link: HTMLAnchorElement | undefined;
  const showLink = () => {
    link = fallbackLink(route, document, placeCaretAfter);
    dom.replaceChildren(link);
  };
  if (named?.version === undefined) {
    showLink();
  } else {
    const img = document.createElement("img");
    img.setAttribute("alt", picture.caption);
    img.setAttribute("loading", "lazy");
    img.className = pictureClassName;
    // The listener and the picture's place in the wrapper come before `src`, so an error the
    // browser raises at once is caught and replaces the picture rather than preceding it.
    img.addEventListener("error", showLink);
    dom.append(img);
    img.setAttribute("src", artifactVersionPath(named.owner, named.slug, named.version));
  }
  return {
    dom,
    // A click on the link is a click on a link, not ProseMirror's node-selection gesture; events
    // on the picture are the editor's, so clicking it selects it.
    stopEvent: (event) =>
      link !== undefined && event.target instanceof Node && link.contains(event.target),
  };
}
