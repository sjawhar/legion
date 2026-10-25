import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView, NodeView, NodeViewConstructor } from "@milkdown/kit/prose/view";

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
 * `href` (a middle click uses it) and `data-dispatch-href` for the host's click handler
 * (`followDispatchLink`, which navigates in the app on a plain click in the live editor and the
 * read-only version view alike) and the hover card. The words are the reference's own, never an
 * author's caption, as `RefLink` shows them until a title resolves. */
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

/** What the editor tells a picture's view: where its caret goes and whether it is editable. */
export interface PictureEditor {
  /** Puts the editor's caret just after the picture node (the comment in `fallbackLink`). */
  placeCaretAfter(): void;
  /** Whether the editor is editable now; read on each click, since a read-only switch keeps the
   * view. */
  editable(): boolean;
}

/** The schema's own DOM for `node`: the `<img>`, inside the anchor of the image's link when the
 * link is one the schema draws (`linked-image.ts` in `@legion/proof-editor`: a safe address or a
 * `dispatch://` target; an unsafe one draws no anchor). */
function schemaDOM(node: ProseMirrorNode, document: Document): HTMLElement {
  const toDOM = node.type.spec.toDOM;
  if (toDOM === undefined) {
    throw new Error(`the ${node.type.name} node has no toDOM`);
  }
  return DOMSerializer.renderSpec(document, toDOM(node)).dom as HTMLElement;
}

/** A picture's link as the browser treats a text link's in the editor. ProseMirror makes a leaf
 * node's DOM `contenteditable="false"`, so the anchor around a picture would be a live link in an
 * editable editor where a text link's anchor, inside the editing host, is not: the browser follows
 * none of its clicks there. A Ctrl/Cmd-click still opens it through proof-editor's
 * `markdownLinkClickPlugin` (which reads the closest `a[href]` of the click and prevents nothing it
 * does not open), and a plain click on a `dispatch://` link has already been taken by the host's
 * `followDispatchLink`, in the capture phase. In a read-only editor the plugin opens the link on a
 * plain click, as it opens a text link. */
function followNoClickWhileEditable(link: HTMLElement, editor: PictureEditor): void {
  link.addEventListener("click", (event) => {
    if (editor.editable()) event.preventDefault();
  });
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
 * grammar refuses - is drawn by the schema's own `toDOM`.
 *
 * A picture with a link (`[![x](i)](u)`, the image's `link` attribute) sits in the anchor the
 * schema's `toDOM` draws for it, with its attributes (`href` sanitized, `title`,
 * `data-dispatch-href` for a `dispatch://` link), or in none for an unsafe link. A Dispatch
 * picture with a link is that anchor around its `<span data-dispatch-doc-picture>`, and its
 * fallback is a `<span>` with the reference's short form rather than `fallbackLink`, so no anchor
 * sits in another: the picture opens its link, never the artifact's page. Every event inside a
 * linked picture is the editor's, as on an unlinked picture: a click selects it, and the link
 * opens as `followNoClickWhileEditable` says.
 */
export function pictureView(
  node: ProseMirrorNode,
  document: Document,
  editor: PictureEditor
): NodeView {
  const picture = pictureReference(node.attrs);
  if (picture === undefined) {
    const dom = schemaDOM(node, document);
    if (dom.nodeName === "A") {
      followNoClickWhileEditable(dom, editor);
    }
    return { dom };
  }
  const { named, route } = picture;
  // The anchor of the picture's link, if the schema draws one. Its `<img>` is not used, so it is
  // drawn without the source: the browser would try to fetch `dispatch://`.
  const drawn = schemaDOM(node.type.create({ ...node.attrs, src: "" }), document);
  const wrapper = drawn.nodeName === "A" ? drawn : undefined;
  const dom = document.createElement("span");
  dom.dataset.dispatchDocPicture = buildDispatchReference(route);
  let link: HTMLAnchorElement | undefined;
  const showFallback = () => {
    if (wrapper === undefined) {
      link = fallbackLink(route, document, () => editor.placeCaretAfter());
      dom.replaceChildren(link);
      return;
    }
    const words = document.createElement("span");
    words.textContent = shortForm(route);
    dom.replaceChildren(words);
  };
  if (named?.version === undefined) {
    showFallback();
  } else {
    const img = document.createElement("img");
    img.setAttribute("alt", picture.caption);
    img.setAttribute("loading", "lazy");
    img.className = pictureClassName;
    // The listener and the picture's place in the wrapper come before `src`, so an error the
    // browser raises at once is caught and replaces the picture rather than preceding it.
    img.addEventListener("error", showFallback);
    dom.append(img);
    img.setAttribute("src", artifactVersionPath(named.owner, named.slug, named.version));
  }
  if (wrapper !== undefined) {
    wrapper.replaceChildren(dom);
    followNoClickWhileEditable(wrapper, editor);
  }
  return {
    dom: wrapper ?? dom,
    // A click on the fallback link is a click on a link, not ProseMirror's node-selection
    // gesture; events on the picture, and anywhere in a linked picture, are the editor's, so
    // clicking it selects it. A `dragstart` inside the link (a drag of a DOM selection made over
    // its words) is left to ProseMirror, which drags the picture node, instead of the drop pasting
    // the browser's own link HTML; `copy` and `cut` stay stopped so a native copy of the selected
    // words works.
    stopEvent: (event) =>
      event.type !== "dragstart" &&
      link !== undefined &&
      event.target instanceof Node &&
      link.contains(event.target),
  };
}

/** `pictureView` as the editor's `image` node view: `placeCaretAfter` dispatches a collapsed
 * `TextSelection` just after the node, the caret a primary-button mousedown on the fallback link
 * places first (the comment in `fallbackLink` says why), and `editable` is the view's own. A
 * changed `image` node is a new view (no `update`), so the `node` captured here is current. */
export function pictureNodeView(view: EditorView): NodeViewConstructor {
  return (node, _view, getPos) =>
    pictureView(node, view.dom.ownerDocument, {
      editable: () => view.editable,
      placeCaretAfter: () => {
        const pos = getPos();
        if (pos === undefined) return;
        view.dispatch(
          view.state.tr.setSelection(TextSelection.create(view.state.doc, pos + node.nodeSize))
        );
      },
    });
}
