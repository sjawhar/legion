import { type ReactNode, useEffect, useRef, useState } from "react";

import { artifactVersionPath } from "../../api/client";
import { borderDefault } from "../../theme/classes";
import { isPlainClick, openPictureLightbox } from "./picture-lightbox";
import { pictureThumbnailClassName, RefLink } from "./RefLink";
import { useReferenceTarget } from "./reference-target";
import { type DispatchReferenceRoute, parseNodeReference, referencedArtifact } from "./routes";

/** A picture's placeholder in a rendered body, holding the `DispatchPicture` a portal renders
 *  into it: the link the engine's serializer wrote, or the element a surface put in its place
 *  (`MarkdownPreview` keeps no link inside the link it sits in). */
export interface PictureAnchor {
  readonly anchor: HTMLElement;
  readonly caption: string;
  readonly key: string;
  readonly route: DispatchReferenceRoute;
}

/** A picture at its own size, scaled down to the column it sits in and never capped in height,
 *  so a wide figure fills the column; a click opens it at full size (`openPictureLightbox`).
 *  `not-prose` keeps Markdown's figure margins off it, so it sits in its line as a pasted picture
 *  is written. The document editor's picture node view (`features/doc/picture-view.ts`) draws
 *  with the same string, so a picture in a spec and one in a comment are one shape. */
export const pictureClassName = `not-prose inline-block h-auto w-auto max-w-full cursor-zoom-in rounded border object-contain align-bottom ${borderDefault}`;

/** Every picture placeholder the engine's serializer wrote into root (`markdown-engine.ts`), with
 *  the caption and the reference its `DispatchPicture` renders. The placeholder's reference is the
 *  one `pictureReference` read as a node's, so it is read as one here too, whatever its length. */
export function collectPictureAnchors(root: HTMLElement): PictureAnchor[] {
  const pictures: PictureAnchor[] = [];
  let index = 0;
  for (const anchor of root.querySelectorAll<HTMLElement>("[data-dispatch-picture]")) {
    const reference = anchor.getAttribute("data-dispatch-picture") ?? "";
    const route = parseNodeReference(reference);
    if (route === undefined) {
      continue;
    }
    pictures.push({
      anchor,
      caption: anchor.getAttribute("data-picture-caption") ?? "",
      key: `picture:${reference}:${index}`,
      route,
    });
    index += 1;
  }
  return pictures;
}

/** The block variant's `<img>`. A plain click opens it at full size; the listener is the
 *  element's own, so it runs before anything listening above the placeholder link (the browser's
 *  navigation, the body's link handling) and keeps the click from them. A modified click is the
 *  link's, which opens the artifact's page. The layer it opened closes when the picture leaves
 *  the page (a body re-rendered or gone, another page). */
function BlockPicture({
  caption,
  onError,
  src,
}: {
  caption: string;
  onError: () => void;
  src: string;
}): ReactNode {
  const picture = useRef<HTMLImageElement>(null);
  useEffect(() => {
    const element = picture.current;
    if (element === null) return;
    let lightbox: HTMLDialogElement | undefined;
    const open = (event: MouseEvent) => {
      if (!isPlainClick(event)) return;
      event.preventDefault();
      event.stopPropagation();
      lightbox = openPictureLightbox(element.ownerDocument, src, caption);
    };
    element.addEventListener("click", open);
    return () => {
      element.removeEventListener("click", open);
      lightbox?.close();
    };
  }, [caption, src]);
  return (
    <img
      alt={caption}
      className={pictureClassName}
      loading="lazy"
      onError={onError}
      ref={picture}
      src={src}
    />
  );
}

/**
 * A picture a body embeds, loaded from its version's same-origin bytes route as the reader's
 * browser loads any page resource: the version the source pins, else the artifact's latest, read
 * from the summary its link would read (`useReferenceTarget`'s `picture`), so a source written
 * without `@vN` shows its newest picture and follows a new version when one is uploaded. `block`
 * shows it at its own size, scaled down to the column's width, and opens it at full size on a
 * click; `inline` (a one-line place: an option label, a clamped preview) shows the 40 px thumbnail
 * beside its caption. Until a version-less source's summary is read, and for a target that is no
 * picture the browser can draw - a document or another file referenced with the picture syntax,
 * a version whose bytes fail - it reads as a plain reference to it, its `RefLink`.
 */
export function DispatchPicture({
  caption,
  route,
  variant,
}: {
  caption: string;
  route: DispatchReferenceRoute;
  variant: "block" | "inline";
}): ReactNode {
  const [failed, setFailed] = useState(false);
  const named = referencedArtifact(route);
  const pinned = named?.version;
  const latest = useReferenceTarget(
    named !== undefined && pinned === undefined ? route : undefined,
    { text: false }
  ).picture;
  const src =
    named === undefined
      ? undefined
      : pinned === undefined
        ? latest
        : artifactVersionPath(named.owner, named.slug, pinned);
  if (failed || src === undefined) {
    return <RefLink route={route} />;
  }
  return variant === "inline" ? (
    <>
      <img
        alt=""
        className={pictureThumbnailClassName}
        loading="lazy"
        onError={() => setFailed(true)}
        src={src}
      />{" "}
      {caption}
    </>
  ) : (
    <BlockPicture caption={caption} onError={() => setFailed(true)} src={src} />
  );
}
