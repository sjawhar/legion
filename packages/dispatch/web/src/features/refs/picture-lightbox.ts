import { backdrop80 } from "../../theme/classes";

/** The full-screen layer a picture opens in: the whole viewport, the picture centred on a dark
 *  backdrop and scaled down only as far as the screen needs. */
const lightboxClassName = `fixed inset-0 z-50 m-0 h-full max-h-none w-full max-w-none cursor-zoom-out border-0 p-4 ${backdrop80}`;
const lightboxFrameClassName = "flex h-full w-full items-center justify-center";
const lightboxPictureClassName = "max-h-full max-w-full object-contain";

/** Whether a click on a picture is the plain primary click that opens it at full size. A click
 *  with a modifier key, or with another button, keeps the browser's own meaning (a new tab for
 *  the link a rendered body puts around the picture). */
export function isPlainClick(event: MouseEvent): boolean {
  return event.button === 0 && !event.altKey && !event.ctrlKey && !event.metaKey && !event.shiftKey;
}

/**
 * Opens the picture at `src` at full size over the page, in a modal `<dialog>` appended to the
 * body: as large as its own pixels, never larger than the screen. A click anywhere or Escape
 * closes it, and the browser gives the focus back to what held it. Only one is open at a time: a
 * second call while one is open (a click that reached a picture before the first layer covered
 * it) answers the open one. Plain DOM, so the rendered bodies' `DispatchPicture` and the document
 * editors' picture node view open the same layer. The bytes route serves a picture as an
 * attachment, so opening its URL in a tab would download it; the `<img>` here draws it as the
 * page's own pictures do.
 */
export function openPictureLightbox(
  document: Document,
  src: string,
  caption: string
): HTMLDialogElement {
  const open = document.querySelector<HTMLDialogElement>('dialog[data-testid="picture-lightbox"]');
  if (open !== null) {
    return open;
  }
  const dialog = document.createElement("dialog");
  dialog.className = lightboxClassName;
  dialog.setAttribute("aria-label", caption === "" ? "Picture" : caption);
  dialog.dataset.testid = "picture-lightbox";
  const frame = document.createElement("div");
  frame.className = lightboxFrameClassName;
  const picture = document.createElement("img");
  picture.className = lightboxPictureClassName;
  picture.setAttribute("alt", caption);
  picture.setAttribute("src", src);
  frame.append(picture);
  dialog.append(frame);
  dialog.addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();
  return dialog;
}
