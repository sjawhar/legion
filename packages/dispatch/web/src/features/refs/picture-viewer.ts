import type { default as PhotoSwipe, SlideData } from "photoswipe";

import { importWhenOnline } from "../shell/DeploymentResilience";

/** Whether a click is the plain primary click that opens a picture at full size. A click with a
 *  modifier key, or with another button, keeps the browser's own meaning (a new tab for the link
 *  a rendered body puts around the picture). It reads the five answers every click carries, so
 *  the DOM's `MouseEvent`, React's synthetic one and a document's own `ReferenceClick`
 *  (`features/doc/reference-click.ts`, which asks the same of a click on a link) all satisfy
 *  it. */
export function isPlainClick(event: {
  readonly altKey: boolean;
  readonly button: number;
  readonly ctrlKey: boolean;
  readonly metaKey: boolean;
  readonly shiftKey: boolean;
}): boolean {
  return event.button === 0 && !event.altKey && !event.ctrlKey && !event.metaKey && !event.shiftKey;
}

/** What the caller that opened the view holds: the React picture closes it when it unmounts, the
 *  editors' node view when it is destroyed. `close()` takes the view off the page whenever it is
 *  called - while the library is still being imported, while the view is animating open, and
 *  once it is open - so a picture that leaves the page at any of those moments leaves no view
 *  behind it. */
export interface PictureViewer {
  close: () => void;
}

/** The surface whose pictures a reader steps through: one document (`ProofDocument` and
 *  `VersionView`'s `article.dispatch-doc`) or one rendered body (`MarkdownBody`'s
 *  `markdownClassName`, `features/refs/markdown-engine.ts`). A picture under neither - the
 *  artifact page draws one of its own - is a set of one, where the viewer shows no position and
 *  no step control (PhotoSwipe's `pswp--one-slide` hides its counter and its arrows). */
const READING_SURFACE = "article.dispatch-doc, .dispatch-markdown";

/** Every `<img>` a Dispatch picture's placeholder holds: `[data-dispatch-picture]` in a rendered
 *  body (`markdown-serializer.ts`), `[data-dispatch-doc-picture]` in an editor
 *  (`features/doc/picture-view.ts`). */
const PLACEHOLDER_PICTURE = "[data-dispatch-picture] img, [data-dispatch-doc-picture] img";

/** The thumbnails among them: the 40 px square a line holds beside its title, which opens the
 *  artifact's page rather than this view. Each drawing place stamps the size it drew at on the
 *  element itself (`ArtifactMedia.tsx`, `features/doc/picture-view.ts`), never on the class
 *  alone, which a caller adds its own to. */
const THUMBNAIL_PICTURE = '[data-dispatch-picture-size="thumbnail"]';

/** The viewer that is open, whose library is still being imported, or that is still animating
 *  itself off the page: one at a time. The slot frees on PhotoSwipe's `destroy` (`release`), so
 *  a click while one is closing opens nothing rather than a second view beside it. */
let viewing: ViewerSession | undefined;

interface ViewerSession {
  /** Whether the caller closed the view. A `close()` PhotoSwipe cannot carry out yet - one
   *  before the import finishes, one during the opening animation - records it here, and
   *  whichever of those two moments comes next reads it and closes the view (`draw`). */
  closed: boolean;
  viewer: PhotoSwipe | undefined;
}

/**
 * Opens the picture `clicked` draws over the page: as large as the window, zoomable to its own
 * pixels and pannable there, with the pictures of its reading surface behind the step controls
 * and the `2 of 3` counter. Escape, the close control and a click outside the picture close it,
 * and the browser gives the focus back to what held it. Only one viewer is open at a time: a
 * call while one is open, while one is still importing the library, or while one is animating
 * itself off the page opens nothing and answers undefined, so only the caller that opened it
 * holds it and closes it when its picture leaves the page. Plain DOM, so the rendered bodies'
 * `DispatchPicture` and the document editors' picture node view open the same viewer.
 */
export function openPictureViewer(clicked: HTMLImageElement): PictureViewer | undefined {
  if (viewing !== undefined) {
    return undefined;
  }
  const session: ViewerSession = { closed: false, viewer: undefined };
  viewing = session;
  // The set is read at the click, from the page as the reader clicked it.
  const pictures = surfacePictures(clicked);
  void draw(session, pictures.map(pictureData), Math.max(0, pictures.indexOf(clicked)));
  return {
    close: () => {
      session.closed = true;
      if (session.viewer === undefined) {
        // No viewer to close yet: the import is still running, and `draw` reads `closed` when
        // it lands. Free the slot here, since an import that never answers - the chunk retried
        // while the browser is offline - would otherwise hold it for the rest of the page.
        release(session);
        return;
      }
      // The slot stays taken until PhotoSwipe's `destroy`, at the end of the closing animation
      // the next line starts (or, during the opening animation, of the one `openingAnimationEnd`
      // starts): until then the view is still on the page, and a second one beside it would be
      // two dialogs over one reader.
      session.viewer.close();
    },
  };
}

/** The pictures of the clicked picture's reading surface, in DOM order, the clicked one among
 *  them. A placeholder can hold the 40 px thumbnail instead of a picture drawn at reading size -
 *  a reference link's, an ask option's, `MarkdownPreview`'s inline variant - and a thumbnail
 *  opens the artifact's page rather than this view, so it is no part of the set: the two wear
 *  the same placeholder attribute, and what tells them apart is the size each drawing place
 *  stamped on the element it drew (`THUMBNAIL_PICTURE`). */
function surfacePictures(clicked: HTMLImageElement): HTMLImageElement[] {
  const surface = clicked.closest(READING_SURFACE);
  if (surface === null) {
    return [clicked];
  }
  const pictures = Array.from(
    surface.querySelectorAll<HTMLImageElement>(PLACEHOLDER_PICTURE)
  ).filter((picture) => !picture.matches(THUMBNAIL_PICTURE));
  return pictures.includes(clicked) ? pictures : [clicked];
}

/** One picture of the set: the address the browser drew it from, its alt text, which the viewer
 *  gives its own `<img>` and so is the picture's accessible name there, and its size, which
 *  PhotoSwipe requires. The size comes from the record the page already holds, the `width` and
 *  `height` the drawing places write from the version's own recorded pair (`drawnBox`), so the
 *  viewer measures no page. A picture drawn without them - a version stored before Dispatch
 *  read a size, a reference pinned below the artifact's latest version - reports its own size
 *  once the browser has drawn it; one below the fold that lazy loading has not drawn reports 0
 *  with neither, and is sized when the reader steps to it (`sizeOnStep`). */
function pictureData(picture: HTMLImageElement): SlideData {
  const box = drawnBox(picture);
  return {
    alt: picture.alt,
    height: box?.height ?? (picture.naturalHeight || undefined),
    src: picture.currentSrc || picture.src,
    width: box?.width ?? (picture.naturalWidth || undefined),
  };
}

/** The size the page wrote on the drawn picture: both attributes or neither, since one of the
 *  two names no shape. A picture with no `width` and `height` answers undefined, as every
 *  picture did before a version recorded its size. */
function drawnBox(picture: HTMLImageElement): { width: number; height: number } | undefined {
  const width = Number(picture.getAttribute("width"));
  const height = Number(picture.getAttribute("height"));
  return width > 0 && height > 0 ? { height, width } : undefined;
}

async function draw(session: ViewerSession, dataSource: SlideData[], index: number): Promise<void> {
  const Viewer = await loadViewer();
  if (Viewer === undefined || session.closed) {
    release(session);
    return;
  }
  const viewer = new Viewer({
    dataSource,
    index,
    // The counter must read `2 of 3`; PhotoSwipe's own separator is ` / `.
    indexIndicatorSep: " of ",
    // The zoom a click, the zoom control and the `z` key reach. PhotoSwipe's own is
    // `Math.min(1, fit * 3)`, so a picture more than three times the window would never reach
    // its own pixels - and its own pixels are what a reader opens a picture to see.
    secondaryZoomLevel: 1,
    // What a tap on a touch screen means. PhotoSwipe's own is `toggle-controls`, which would
    // hide the controls instead of zooming; a phone reader's tap means what a desktop reader's
    // click means, which is `imageClickAction`'s own default.
    tapAction: "zoom-or-close",
    // `bgClickAction` is left at PhotoSwipe's `close`: a click outside the picture closes.
  });
  session.viewer = viewer;
  // PhotoSwipe's `close()` returns having done nothing unless `opener.isOpen` is true, and the
  // opener sets that flag only at the end of the opening animation: `_onAnimationComplete`
  // assigns `isOpen` and then dispatches `openingAnimationEnd`. Without the handler below, a
  // close called inside that third of a second - the node view destroyed, the React picture
  // unmounted - would leave the view on the page for good, over a page whose picture is gone.
  // The event is the earliest moment such a close can take the view off the page, and it cannot
  // drift from the flag, because the one method that sets the flag sends the event.
  viewer.on("openingAnimationEnd", () => {
    if (session.closed) {
      viewer.close();
    }
  });
  viewer.on("destroy", () => release(session));
  sizeOnStep(viewer);
  viewer.init();
}

/** PhotoSwipe's class and its stylesheet, lazily, so neither ships in the entry chunk - the way
 *  `features/doc/editor.ts` imports the editor and its stylesheet. A chunk that fails while the
 *  browser is offline is retried when the network returns (`importWhenOnline`); one that fails
 *  online leaves the click having done nothing, with the reason in the console. The answer's
 *  type is the import's own: a type-only import names the viewer, never the class it is made
 *  from. */
async function loadViewer() {
  try {
    const [photoswipe] = await importWhenOnline(() =>
      Promise.all([import("photoswipe"), import("photoswipe/style.css")])
    );
    return photoswipe.default;
  } catch (error) {
    console.error("Could not open the picture viewer", error);
    return undefined;
  }
}

/** Sizes a picture that reached the viewer with no size at all, at the moment the reader steps
 *  to it: one the page had not drawn when the view opened and whose element carries no recorded
 *  pair either, which is the state of every picture stored before Dispatch read a size.
 *  PhotoSwipe requires predefined image dimensions and starts no load without them, so such a
 *  slide draws nothing until its size is known. `contentActivate` is the event for it - it
 *  fires for the slide the reader is on, at the open and at every step, and not for one the
 *  viewer merely holds beside it - and `refreshSlideContent` draws that slide again from the
 *  item data this fills in. So opening the view fetches nothing the reader does not look at. */
function sizeOnStep(viewer: PhotoSwipe): void {
  const sizing = new Set<number>();
  viewer.on("contentActivate", ({ content }) => {
    if (content.data.width !== undefined || sizing.has(content.index)) {
      return;
    }
    sizing.add(content.index);
    const sized = (width: number, height: number) => {
      content.data.width = width;
      content.data.height = height;
      viewer.refreshSlideContent(content.index);
    };
    const loading = new Image();
    loading.addEventListener("load", () => sized(loading.naturalWidth, loading.naturalHeight));
    // Bytes that fail report no size at all: the window's gives the viewer a slide to draw its
    // own "The image cannot be loaded" in, in place of an empty one.
    loading.addEventListener("error", () => sized(viewer.viewportSize.x, viewer.viewportSize.y));
    loading.src = content.data.src ?? "";
  });
}

function release(session: ViewerSession): void {
  if (viewing === session) {
    viewing = undefined;
  }
}
