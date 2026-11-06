import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView, NodeView, NodeViewConstructor } from "@milkdown/kit/prose/view";
import type { QueryClient } from "@tanstack/react-query";

import { artifactVersionPath } from "../../api/client";
import { pictureClassName } from "../refs/picture-classes";
import { isPlainClick, openPictureViewer, type PictureViewer } from "../refs/picture-viewer";
import type { ReferenceMedia } from "../refs/reference-target";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  pictureReference,
  shortForm,
} from "../refs/routes";
import { type NavigateToReference, openReference, opensReference } from "./reference-click";

/** What the editor a picture is drawn in lends its fallback link: whether its view takes edits
 * right now, and the pair the link opens its artifact inside the app with. The node view is
 * plain DOM, so it is handed these at the editor's mount (`installTypedBlocks`) rather than
 * reading a hook. */
export interface PictureLinkOpener {
  /** Whether the view takes edits at the moment it is asked. The editor library's own
   *  `Open link` card shows in an editable view alone, and Dispatch's card triggers on
   *  `data-dispatch-href` (`refs/RefPreview.tsx`), so the attribute is written in a read-only
   *  view alone: the live editor then shows the editor's card and a past version Dispatch's,
   *  one card each. It is read on every draw rather than taken once, because a mounted editor
   *  is turned read-only and back in place (`ProofDocument.tsx` on a closed issue or a change
   *  of admission) and its node views are not rebuilt. */
  readonly editable: () => boolean;
  readonly navigate: NavigateToReference;
  readonly queryClient: QueryClient;
}

/** Writes Dispatch's hover card onto a picture's fallback link, or takes it off: the card
 *  triggers on `data-dispatch-href`, the editor library's own shows in an editable view, and
 *  one element carries one card. The reference the attribute holds is the wrapper's own
 *  (`data-dispatch-doc-picture`), so a link drawn before a toggle and one drawn after it are
 *  written from the same string. */
function showCard(link: HTMLAnchorElement, reference: string, editable: boolean): void {
  if (editable) delete link.dataset.dispatchHref;
  else link.dataset.dispatchHref = reference;
}

/**
 * Writes each fallback link under `view` the card its editable state calls for, for links drawn
 * before the state changed: `editor.ts` runs this pass from `setReadOnly`, since a mounted
 * editor's read-only state is changed in place and ProseMirror rebuilds no node view for it.
 * The pass reads `view.editable` itself, the same live value `PictureLinkOpener.editable` reads
 * on every draw, so the two cannot disagree about what the editor is.
 */
export function syncFallbackLinkCards(view: EditorView): void {
  const editable = view.editable;
  for (const wrapper of view.dom.querySelectorAll<HTMLElement>("[data-dispatch-doc-picture]")) {
    const reference = wrapper.dataset.dispatchDocPicture;
    const link = wrapper.firstElementChild;
    if (reference === undefined || !(link instanceof HTMLAnchorElement)) continue;
    showCard(link, reference, editable);
  }
}

/** The link a picture falls back to: the reference's short form, with the real SPA path as its
 * `href`, so the keyboard, a middle click, a modified click and the browser's own menu all do
 * what they do on any link. Its own `click` listener opens the artifact inside the app: being
 * the element's own, it runs before anything listening above it, and it is what a past version
 * needs, where proof-editor's `markdownLinkClickPlugin` would otherwise open a new tab. In a
 * read-only view the link also carries `data-dispatch-href`, which is Dispatch's hover card; an
 * editable view leaves it off, since the editor library's own card already shows there and two
 * cards on one element is one too many (`PictureLinkOpener`). The words are the reference's
 * own, never an author's caption, as `RefLink` shows them until a title resolves. */
function fallbackLink(
  route: DispatchReferenceRoute,
  document: Document,
  placeCaretAfter: () => void,
  opener: PictureLinkOpener
): HTMLAnchorElement {
  const link = document.createElement("a");
  link.setAttribute("href", buildReferencePath(route));
  showCard(link, buildDispatchReference(route), opener.editable());
  link.textContent = shortForm(route);
  link.addEventListener("click", (event) => {
    if (!opensReference(event)) return;
    event.preventDefault();
    openReference(route, opener.queryClient, opener.navigate);
  });
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

/** Watches what a picture's reference draws: calls its listener at once and on every change,
 *  with the artifact's latest version for a source that pins none, the kind alone for a source
 *  that pins one, and undefined while the target is unread, unreadable or neither a picture nor
 *  a video (`watchPicturePath` in `refs/reference-target.ts`, bound to the page's query client).
 *  The returned function stops it. */
export type WatchPicture = (
  route: DispatchReferenceRoute,
  onPicture: (media: ReferenceMedia | undefined) => void
) => () => void;

/**
 * The node view of Milkdown's `image` node. The browser cannot fetch `dispatch://`, so a picture
 * whose source names an artifact (`![shot.png](dispatch://KEY/artifact/shot-png@v1)`, the line a
 * pasted picture is written as) is drawn from the same-origin bytes route of the version it pins,
 * or, for a source written without `@vN`, of the latest version `watchPicture` reports - which
 * follows a new version or a recovered read while the view lives - with the caption
 * `pictureReference` reads (`refs/routes.ts`, the rule it shares with `pictureSerializer`): the
 * `alt`, or the artifact's slug when that is empty. An artifact stored under a video type is a
 * `<video controls preload="metadata">` at the same address instead, with the same class, the
 * same failure answer and the same events; it opens no full-size view, since a player is
 * already the whole of what a video does here. The document's markdown is untouched by either:
 * only this view's DOM differs, so a save writes the line it read.
 *
 * The picture is at its own size, scaled down to the column (`pictureClassName`, the reading
 * size the React places draw with too), carrying the version's own width and height where the
 * watch answers them (`setBox`), so the text below it does not move as the bytes arrive, and a
 * plain click opens it at full size (`openPictureViewer`), a view the node view closes when it
 * is destroyed. A version the browser cannot draw (a document, another file, a missing
 * version) errors, and the element becomes `fallbackLink`; a version-less source shows that
 * link while no latest version is known, and a source that names something other than an
 * artifact is that link at once. `placeCaretAfter` puts the editor's caret just after the node,
 * which the link calls on a primary-button mousedown (the comment in `fallbackLink` says why).
 * Any other source - a picture on another website, a relative path, a `dispatch://` string the
 * reference grammar refuses - is drawn by the schema's own `toDOM`, as today.
 */
export function pictureView(
  node: ProseMirrorNode,
  document: Document,
  placeCaretAfter: () => void,
  watchPicture: WatchPicture,
  opener: PictureLinkOpener
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
  let viewer: PictureViewer | undefined;
  // What the wrapper shows: the address drawn and whether it is drawn as a picture or played,
  // both undefined while it shows the link, and the picture itself while one is drawn, which a
  // later report writes a size onto. A report of what is already shown is skipped, and one
  // after a failed load draws again.
  let shown: string | undefined;
  let shownKind: ReferenceMedia["kind"] | undefined;
  let drawn: HTMLImageElement | undefined;
  const showLink = () => {
    shown = undefined;
    shownKind = undefined;
    drawn = undefined;
    link = fallbackLink(route, document, placeCaretAfter, opener);
    dom.replaceChildren(link);
  };
  /** Writes the version's own size onto the drawn picture, so the editor reserves its box before
   *  the bytes arrive and the paragraph below it does not move as they land. A reference that
   *  answers no pair - a version stored before Dispatch read a size, a source pinned below the
   *  artifact's latest version - leaves the element sized by its bytes, as it always was. The
   *  pair can land after the picture is drawn, where a pinned source drew before its summary
   *  was read, and sizes the element then rather than drawing it again and starting its load
   *  over. */
  const setBox = (media: ReferenceMedia | undefined) => {
    if (drawn === undefined || media?.width === undefined || media.height === undefined) return;
    drawn.setAttribute("width", String(media.width));
    drawn.setAttribute("height", String(media.height));
  };
  const showMedia = (kind: ReferenceMedia["kind"], src: string, media?: ReferenceMedia) => {
    let element: HTMLImageElement | HTMLVideoElement;
    drawn = undefined;
    if (kind === "video") {
      const player = document.createElement("video");
      player.setAttribute("controls", "");
      player.setAttribute("preload", "metadata");
      if (picture.caption !== "") player.setAttribute("aria-label", picture.caption);
      element = player;
    } else {
      const img = document.createElement("img");
      img.setAttribute("alt", picture.caption);
      img.setAttribute("loading", "lazy");
      // ProseMirror selects the node on mousedown and mouseup; the click after them opens it.
      // Only the default is prevented: the editor still sees the click that follows its
      // selection. A player opens nothing, so it takes no click of its own.
      img.addEventListener("click", (event) => {
        if (!isPlainClick(event)) return;
        event.preventDefault();
        viewer = openPictureViewer(img) ?? viewer;
      });
      element = img;
      drawn = img;
    }
    element.className = pictureClassName;
    // The listener and the element's place in the wrapper come before `src`, so an error the
    // browser raises at once is caught and replaces it rather than preceding it. An element a
    // newer version already replaced falls back to nothing.
    element.addEventListener("error", () => {
      if (element.parentNode === dom) showLink();
    });
    link = undefined;
    shown = src;
    shownKind = kind;
    setBox(media);
    dom.replaceChildren(element);
    element.setAttribute("src", src);
  };
  let stopWatching = () => {};
  if (named === undefined) {
    showLink();
  } else if (named.version !== undefined) {
    // A pinned source draws from the address it names, with no wait on a read: the version is
    // in the reference. Its type is not, so the watch is what tells a video from a picture - it
    // reports what the page has already read in this same task, before anything is drawn, and a
    // later report (the summary landing, a failed read recovering) turns a picture into a player
    // where it says video. It never takes the element away: a pinned source has always drawn the
    // bytes it names and fallen back to the link only where the browser refused them.
    const src = artifactVersionPath(named.owner, named.slug, named.version);
    stopWatching = watchPicture(route, (media) => {
      const kind = media?.kind ?? "picture";
      // A report that only brings the size - the summary landing behind a picture already drawn
      // from the address the reference names - writes the box on it rather than drawing again.
      if (kind === shownKind && src === shown) {
        setBox(media);
        return;
      }
      showMedia(kind, src, media);
    });
    if (shown === undefined) showMedia("picture", src);
  } else {
    showLink();
    stopWatching = watchPicture(route, (media) => {
      if (media?.src === shown && media?.kind === shownKind) return;
      if (media === undefined) showLink();
      else showMedia(media.kind, media.src, media);
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
    // A view destroyed while the viewer's library is still being imported ends with nothing
    // open: `close()` before the import lands calls that open off.
    destroy: () => {
      stopWatching();
      viewer?.close();
    },
  };
}

/** `pictureView` as the editor's `image` node view: `placeCaretAfter` dispatches a collapsed
 * `TextSelection` just after the node, the caret a primary-button mousedown on the fallback link
 * places first (the comment in `fallbackLink` says why). A changed `image` node is a new view (no
 * `update`), so the `node` captured here is current. */
export function pictureNodeView(
  view: EditorView,
  watchPicture: WatchPicture,
  opener: PictureLinkOpener
): NodeViewConstructor {
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
      watchPicture,
      opener
    );
}
