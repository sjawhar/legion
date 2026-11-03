import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView, NodeView, NodeViewConstructor } from "@milkdown/kit/prose/view";

import { artifactVersionPath } from "../../api/client";
import { pictureClassName } from "../refs/DispatchPicture";
import { isPlainClick, openPictureLightbox } from "../refs/picture-lightbox";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  pictureReference,
  shortForm,
} from "../refs/routes";

/** The link a picture falls back to: the reference's short form, with the real SPA path as its
 * `href` (in the read-only version view the click is taken by proof-editor's
 * `markdownLinkClickPlugin`, which opens the page in a new tab; a Ctrl/Cmd-click does the same in
 * the live editor; a middle click uses the real `href`) and `data-dispatch-href` for the live
 * editor's click handler and the hover card. The words are the reference's own, never an author's
 * caption, as `RefLink` shows them until a title resolves. */
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

/** Watches the bytes route of a version-less picture's latest version: calls its listener at once
 *  and on every change, with undefined while the target is unread, unreadable or no picture the
 *  browser can draw, and returns the function that stops (`watchPicturePath` in
 *  `refs/reference-target.ts`, bound to the page's query client). */
export type WatchPicture = (
  route: DispatchReferenceRoute,
  onPicture: (src: string | undefined) => void
) => () => void;

/**
 * The node view of Milkdown's `image` node. The browser cannot fetch `dispatch://`, so a picture
 * whose source names an artifact (`![shot.png](dispatch://KEY/artifact/shot-png@v1)`, the line a
 * pasted picture is written as) is drawn from the same-origin bytes route of the version it pins,
 * or, for a source written without `@vN`, of the latest version `watchPicture` reports - which
 * follows a new version or a recovered read while the view lives - with the caption
 * `pictureReference` reads (`refs/routes.ts`, the rule it shares with `pictureSerializer`): the
 * `alt`, or the artifact's slug when that is empty. The picture is at its own size, scaled down to
 * the column (`pictureClassName`), and a plain click opens it at full size
 * (`openPictureLightbox`), a layer the view closes when it is destroyed. A version the browser
 * cannot draw (a document, another file, a missing version) errors, and the picture becomes
 * `fallbackLink`; a version-less source shows that link while no latest version is known, and a
 * source that names something other than an artifact is that link at once. `placeCaretAfter` puts
 * the editor's caret just after the node, which the link calls on a primary-button mousedown (the
 * comment in `fallbackLink` says why). Any other source - a picture on another website, a relative
 * path, a `dispatch://` string the reference grammar refuses - is drawn by the schema's own
 * `toDOM`, as today.
 */
export function pictureView(
  node: ProseMirrorNode,
  document: Document,
  placeCaretAfter: () => void,
  watchPicture: WatchPicture
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
  let lightbox: HTMLDialogElement | undefined;
  // The path the wrapper shows a picture of, undefined while it shows the link: a report of the
  // same path is skipped, and one after a failed load draws again.
  let shown: string | undefined;
  const showLink = () => {
    shown = undefined;
    link = fallbackLink(route, document, placeCaretAfter);
    dom.replaceChildren(link);
  };
  const showPicture = (src: string) => {
    const img = document.createElement("img");
    img.setAttribute("alt", picture.caption);
    img.setAttribute("loading", "lazy");
    img.className = pictureClassName;
    // The listener and the picture's place in the wrapper come before `src`, so an error the
    // browser raises at once is caught and replaces the picture rather than preceding it. A
    // picture a newer version already replaced falls back to nothing.
    img.addEventListener("error", () => {
      if (img.parentNode === dom) showLink();
    });
    // ProseMirror selects the node on mousedown and mouseup; the click after them opens it.
    img.addEventListener("click", (event) => {
      if (!isPlainClick(event)) return;
      event.preventDefault();
      lightbox = openPictureLightbox(document, src, picture.caption) ?? lightbox;
    });
    link = undefined;
    shown = src;
    dom.replaceChildren(img);
    img.setAttribute("src", src);
  };
  let stopWatching = () => {};
  if (named === undefined) {
    showLink();
  } else if (named.version !== undefined) {
    showPicture(artifactVersionPath(named.owner, named.slug, named.version));
  } else {
    showLink();
    stopWatching = watchPicture(route, (src) => {
      if (src === shown) return;
      if (src === undefined) showLink();
      else showPicture(src);
    });
  }
  return {
    dom,
    // A click on the link is a click on a link, not ProseMirror's node-selection gesture; events
    // on the picture are the editor's, so clicking it selects it. A `dragstart` inside the link (a
    // drag of a DOM selection made over its words) is left to ProseMirror, which drags the picture
    // node, instead of the drop pasting the browser's own link HTML; `copy` and `cut` stay stopped
    // so a native copy of the selected words works.
    stopEvent: (event) =>
      event.type !== "dragstart" &&
      link !== undefined &&
      event.target instanceof Node &&
      link.contains(event.target),
    destroy: () => {
      stopWatching();
      lightbox?.close();
    },
  };
}

/** `pictureView` as the editor's `image` node view: `placeCaretAfter` dispatches a collapsed
 * `TextSelection` just after the node, the caret a primary-button mousedown on the fallback link
 * places first (the comment in `fallbackLink` says why). A changed `image` node is a new view (no
 * `update`), so the `node` captured here is current. */
export function pictureNodeView(view: EditorView, watchPicture: WatchPicture): NodeViewConstructor {
  return (node, _view, getPos) =>
    pictureView(
      node,
      view.dom.ownerDocument,
      () => {
        const pos = getPos();
        if (pos === undefined) return;
        view.dispatch(
          view.state.tr.setSelection(TextSelection.create(view.state.doc, pos + node.nodeSize))
        );
      },
      watchPicture
    );
}
