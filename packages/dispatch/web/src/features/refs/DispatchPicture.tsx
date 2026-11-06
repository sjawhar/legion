import type { ReactNode } from "react";

import { ArtifactMedia } from "./ArtifactMedia";
import { RefLink } from "./RefLink";
import { usePinnedMedia, useReferenceTarget } from "./reference-target";
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

/**
 * What a body's picture line (`![name](dispatch://…)`) draws: the version's same-origin bytes,
 * loaded as the reader's browser loads any page resource - the version the source pins, else
 * the artifact's latest, read from the summary its link would read (`useReferenceTarget`'s
 * `picture`), so a source written without `@vN` shows its newest version and follows a new one
 * when it is uploaded. A video artifact draws a player wherever a picture is drawn at reading
 * size, from the same address; `block` draws either at its own size, scaled down to the
 * column's width, and opens a picture at full size on a click. `inline` (a one-line place: an
 * option label, a clamped preview) shows a picture's 40 px thumbnail beside its caption, and
 * reads a video as its reference, since a player is no one-line thing. Until a version-less
 * source's summary is read, and for a target the browser neither draws nor plays - a document
 * or another file referenced with the picture syntax - it reads as a plain reference to it, its
 * `RefLink`, which is also what a version whose bytes fail falls back to (`ArtifactMedia`).
 */
export function DispatchPicture({
  anchor,
  caption,
  route,
  variant,
}: {
  /** The placeholder this is portalled into: the link the serializer wrote, or the element a
   *  surface put in its place. A player leaves that link where it is, so the keyboard reaches
   *  the artifact's page through it; the address comes off only for the length of a pointer
   *  press, so a press on the player's controls plays instead of opening that page
   *  (`ArtifactMedia`). */
  anchor?: HTMLElement;
  caption: string;
  route: DispatchReferenceRoute;
  variant: "block" | "inline";
}): ReactNode {
  const named = referencedArtifact(route);
  // The summary read behind every reference answers both forms: for a version-less source the
  // latest version's address, for a pinned one the address the source itself names with the
  // type the summary carries, which is what tells a pinned video from a pinned picture. The
  // pinned read is what draws before it lands - the version is in the reference, so a picture
  // never waits on a read to appear - and what draws where the summary cannot be read at all.
  const target = useReferenceTarget(named === undefined ? undefined : route, {
    text: false,
  }).picture;
  const pinned = usePinnedMedia(route);
  const media = target ?? pinned;
  // The placeholder is the link the serializer wrote, so the fallback hands the reader back
  // exactly that link, its address the one `RefLink` keeps on it.
  const link = <RefLink anchor={anchor} route={route} />;
  if (media === undefined || (variant === "inline" && media.kind === "video")) {
    return link;
  }
  return variant === "inline" ? (
    <ArtifactMedia
      caption=""
      fallback={link}
      kind="picture"
      src={media.src}
      variant="thumbnail"
      words={caption}
    />
  ) : (
    // The pair the reference answers with, where it has one: the version's own size, which a
    // picture at the artifact's latest version carries and a pinned older one does not
    // (`ReferenceMedia`). The thumbnail above takes none: its class fixes its box.
    <ArtifactMedia
      caption={caption}
      fallback={link}
      height={media.height}
      host={anchor}
      kind={media.kind}
      src={media.src}
      variant="block"
      width={media.width}
    />
  );
}
