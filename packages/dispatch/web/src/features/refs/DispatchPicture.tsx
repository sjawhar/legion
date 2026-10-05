import { type ReactNode, useState } from "react";

import { artifactVersionPath } from "../../api/client";
import { borderDefault } from "../../theme/classes";
import { pictureThumbnailClassName, RefLink } from "./RefLink";
import { type DispatchReferenceRoute, parseDispatchReference, referencedArtifact } from "./routes";

/** A picture's placeholder in a rendered body, holding the `DispatchPicture` a portal renders
 *  into it: the link the engine's serializer wrote, or the element a surface put in its place
 *  (`MarkdownPreview` keeps no link inside the link it sits in). */
export interface PictureAnchor {
  readonly anchor: HTMLElement;
  readonly caption: string;
  readonly key: string;
  readonly route: DispatchReferenceRoute;
}

/** A picture scaled to the column it sits in and never taller than 24rem; `not-prose` keeps
 *  Markdown's figure margins off it, so it sits in its line as a pasted picture is written. */
const pictureClassName = `not-prose inline-block max-h-96 w-auto max-w-full rounded border object-contain align-bottom ${borderDefault}`;

/** Every picture placeholder the engine's serializer wrote into root (`markdown-engine.ts`), with
 *  the caption and the reference its `DispatchPicture` renders. */
export function collectPictureAnchors(root: HTMLElement): PictureAnchor[] {
  const pictures: PictureAnchor[] = [];
  let index = 0;
  for (const anchor of root.querySelectorAll<HTMLElement>("[data-dispatch-picture]")) {
    const reference = anchor.getAttribute("data-dispatch-picture") ?? "";
    const route = parseDispatchReference(reference);
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
 * A picture a body embeds, loaded from its version's same-origin bytes route as the reader's
 * browser loads any page resource. `block` shows it at the column's width, capped in height;
 * `inline` (a one-line place: an option label, a clamped preview) shows the 40 px thumbnail beside
 * its caption. A version whose bytes are no picture the browser can draw - a document or another
 * file referenced with the picture syntax - falls back to the reference's title, as a plain
 * reference to it reads.
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
  if (failed || named?.version === undefined) {
    return <RefLink route={route} />;
  }
  const src = artifactVersionPath(named.owner, named.slug, named.version);
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
    <img
      alt={caption}
      className={pictureClassName}
      loading="lazy"
      onError={() => setFailed(true)}
      src={src}
    />
  );
}
