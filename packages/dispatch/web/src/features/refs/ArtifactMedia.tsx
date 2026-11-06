import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";

import { pictureClassName, pictureThumbnailClassName } from "./picture-classes";
import { isPlainClick, openPictureViewer, type PictureViewer } from "./picture-viewer";

/**
 * The one unit every React place draws an artifact's version with: a rendered body's picture
 * line and its plain reference (`DispatchPicture`, `RefLink`), the artifact page
 * (`ArtifactBlobView`) and the Artifacts tab's row. It owns the class of each size
 * (`picture-classes.ts`), the lazy loading, the failure answer and the click that opens a
 * picture at full size, so a place decides only what to draw and from where. The two document
 * editors draw the same two things in plain DOM, through the same class declarations and the
 * same failure answer, because a React component cannot be mounted in a ProseMirror node view
 * (`features/doc/picture-view.ts`).
 *
 * `src` is the version's same-origin bytes route (`artifactVersionPath`, the drawing address;
 * `artifactVersionUrl` is the download one), which the reader's browser loads with their own
 * cookie. A video is `<video controls preload="metadata">`: the browser reads its first frames
 * and its duration, and the reader asks for the rest. Once the browser refuses the address - a
 * version it cannot decode, a version the server does not serve - the element goes and
 * `fallback` is drawn in its place, so no place shows a broken-image glyph or an element
 * reporting a `naturalWidth` of zero. What failed is kept as the address and what was drawn
 * from it, not a flag: a reference names the latest version, so a version uploaded since gets
 * its own load instead of inheriting the last one's failure, and a picture that failed at an
 * address a summary then answers for as a video gets its player, since a video's bytes are not
 * the picture that the browser refused.
 */
export function ArtifactMedia({
  caption,
  className,
  fallback,
  height,
  host,
  kind,
  src,
  variant,
  width,
  words,
}: {
  /** The element's own accessible name: an `<img>`'s `alt`, a player's label. A thumbnail that
   *  stands beside its `words` is named by them and carries `""` here, as a decorative picture
   *  does; one that stands alone in a row of its own (the Artifacts tab) is named here. */
  caption: string;
  /** What the place adds to its size's own class: the artifact page's own width and border.
   *  Left out everywhere else. It is the class alone that it adds to: the size the element was
   *  drawn at is stamped on the element itself (`data-dispatch-picture-size`, written beside
   *  the class here and by the editors' node view), which is what `picture-viewer.ts` reads to
   *  keep a thumbnail out of the set a reader steps through. */
  className?: string;
  /** What is drawn once the browser refuses the address: a link to the artifact, never a broken
   *  element. */
  fallback: ReactNode;
  /** The picture's own size in pixels, which the drawn `<img>` carries as its `width` and
   *  `height` attributes, so the browser reserves its box before the bytes arrive and the text
   *  below it does not move as they land. Handed together or not at all, and only for a picture
   *  drawn at reading size: a player sizes itself, and the 40 px square's own class fixes its
   *  box, so neither place hands them. Left out for a version whose size Dispatch has not read
   *  (`backfill-dimensions` fills those) and for a reference pinned below the artifact's latest
   *  version, where the element is sized by the bytes as it always was. */
  height?: number;
  width?: number;
  /** The element a rendered body portals this into, when it is one: the `<a>` to the artifact's
   *  page that holds a picture line or a plain reference. */
  host?: HTMLElement;
  /** What the version is: a picture the browser draws, or a video it plays. A reference
   *  answers it as `ReferenceMedia["kind"]` (`reference-target.ts`); the artifact page and the
   *  Artifacts tab read it from the version's own stored type (`isStoredVideo`). */
  kind: "picture" | "video";
  src: string;
  variant: "block" | "thumbnail";
  /** The words a thumbnail stands beside - a picture line's caption, a reference's title - which
   *  name it and go with it when the browser refuses it, since `fallback` then says it all. */
  words?: ReactNode;
}): ReactNode {
  const [failed, setFailed] = useState<{ kind: "picture" | "video"; src: string }>();
  const picture = useRef<HTMLImageElement>(null);
  const drawn = failed?.src !== src || failed.kind !== kind;
  /** A plain click opens the picture at full size; the listener is the element's own, so it runs
   *  before anything listening above the placeholder link (the browser's navigation, the body's
   *  link handling) and keeps the click from them. A modified click is the link's, which opens
   *  the artifact's page. The viewer it opened closes when the picture leaves the page (a body
   *  re-rendered or gone, another page, a load that failed), whether or not its library has
   *  arrived by then. The viewer reads the picture's address, its caption and the surface's
   *  other pictures off the element itself, so none of them is a dependency of this effect. */
  const opensViewer = variant === "block" && kind === "picture" && drawn;
  useEffect(() => {
    const element = picture.current;
    if (element === null || !opensViewer) return;
    let viewer: PictureViewer | undefined;
    const open = (event: MouseEvent) => {
      if (!isPlainClick(event)) return;
      event.preventDefault();
      event.stopPropagation();
      viewer = openPictureViewer(element) ?? viewer;
    };
    element.addEventListener("click", open);
    return () => {
      element.removeEventListener("click", open);
      viewer?.close();
    };
  }, [opensViewer]);
  usePlayerHost(host, kind === "video" && drawn);

  if (!drawn) {
    return <>{fallback}</>;
  }
  const sizeClassName = variant === "block" ? pictureClassName : pictureThumbnailClassName;
  const classes = className === undefined ? sizeClassName : `${sizeClassName} ${className}`;
  if (kind === "video") {
    return (
      // Dispatch stores an uploaded video's bytes and nothing else, so there is no caption
      // track for a `<track>` to point at.
      // biome-ignore lint/a11y/useMediaCaption: an uploaded video has no caption track.
      <video
        aria-label={caption === "" ? undefined : caption}
        className={classes}
        controls
        data-dispatch-picture-size={variant}
        onError={() => setFailed({ kind, src })}
        preload="metadata"
        src={src}
      />
    );
  }
  // `width` and `height` are the version's own, where the place knows them: the box the browser
  // reserves before the bytes arrive. A place that knows neither hands neither, and the element
  // is drawn exactly as it was before Dispatch recorded a picture's size.
  const image = (
    <img
      alt={caption}
      className={classes}
      data-dispatch-picture-size={variant}
      height={height}
      loading="lazy"
      onError={() => setFailed({ kind, src })}
      ref={picture}
      src={src}
      width={width}
    />
  );
  return words === undefined ? (
    image
  ) : (
    <>
      {image} {words}
    </>
  );
}

/** Keeps a pointer press on a player from opening the page the link it is drawn inside goes to,
 *  and leaves that link where a keyboard reader finds it: a rendered body portals both reference
 *  forms into an `<a>` to the artifact's page, and a press on a player's controls inside a link
 *  that holds its address opens that page instead of playing. The anchor keeps its `href`, so it
 *  takes the focus, is named by the player it holds and opens the artifact's page on Enter,
 *  exactly as the link around a picture does; the address comes off for the length of a pointer
 *  press that reached the anchor and goes back on the press's own `pointerup`, since what decides
 *  whether a click activates a link is the address the anchor holds when that click is
 *  dispatched. The press is read in the capture phase, ahead of the controls' own handling, and
 *  the release on the window, which no handler of theirs can keep it from; neither is cancelled,
 *  so the control the press landed on does what it always did. Chromium and Firefox dispatch
 *  neither event into the page's own tree for a press on the controls, so there the address
 *  never moves and the controls are simply the controls; an engine that does dispatch them
 *  dispatches both, and the press is the player's rather than the link's. A host that is no
 *  anchor (a preview puts a `<span>` where it keeps no link inside its own) is left alone. */
function usePlayerHost(host: HTMLElement | undefined, playing: boolean): void {
  useLayoutEffect(() => {
    if (!playing || !(host instanceof HTMLAnchorElement)) return;
    /** The address while a press holds it off the anchor, and what says a press is running. */
    let held: string | null = null;
    const disarm = () => {
      const href = host.getAttribute("href");
      if (held !== null || href === null) return;
      held = href;
      host.removeAttribute("href");
    };
    const rearm = () => {
      if (held === null) return;
      host.setAttribute("href", held);
      held = null;
    };
    // A press's click is dispatched in the same task as its release, so the address goes back
    // in the next one: the click finds no link, and the next key press finds one.
    const rearmAfterTheClick = () => setTimeout(rearm, 0);
    host.addEventListener("pointerdown", disarm, true);
    window.addEventListener("pointerup", rearmAfterTheClick, true);
    window.addEventListener("pointercancel", rearmAfterTheClick, true);
    return () => {
      host.removeEventListener("pointerdown", disarm, true);
      window.removeEventListener("pointerup", rearmAfterTheClick, true);
      window.removeEventListener("pointercancel", rearmAfterTheClick, true);
      rearm();
    };
  }, [host, playing]);
}
