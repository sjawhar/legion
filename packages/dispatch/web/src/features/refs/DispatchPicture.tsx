import { DOMSerializer, type Schema } from "prosemirror-model";
import { type ReactNode, useState } from "react";

import { artifactVersionPath } from "../../api/client";
import { borderDefault } from "../../theme/classes";
import { pictureThumbnailClassName, RefLink } from "./RefLink";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  parseDispatchReference,
  referencedArtifact,
} from "./routes";

/** A picture's link in a rendered body, holding the `DispatchPicture` a portal renders into it. */
export interface PictureAnchor {
  readonly anchor: HTMLAnchorElement;
  readonly caption: string;
  readonly key: string;
  readonly route: DispatchReferenceRoute;
}

/** A picture scaled to the column it sits in and never taller than 24rem; `not-prose` keeps
 *  Markdown's figure margins off it, so it sits in its line as a pasted picture is written. */
const pictureClassName = `not-prose inline-block max-h-96 w-auto max-w-full rounded border object-contain align-bottom ${borderDefault}`;

const serializers = new WeakMap<Schema, DOMSerializer>();

/**
 * Proof's DOM serializer, except for a picture whose address is a Dispatch reference: the browser
 * cannot fetch `dispatch://`, so an `image` node naming an artifact at a version
 * (`![shot.png](dispatch://KEY/artifact/shot-png@v1)`, the syntax a pasted picture is written in)
 * becomes an empty link to the artifact's page, marked `data-dispatch-picture`, for
 * `collectPictureAnchors` to hand to a `DispatchPicture`. One that pins no version, or names
 * something other than an artifact, is a reference like any other and becomes a link
 * `collectReferenceAnchors` titles. A picture on another website, or an address that is no
 * reference, is serialized as Proof serializes it.
 */
export function pictureSerializer(schema: Schema): DOMSerializer {
  const cached = serializers.get(schema);
  if (cached !== undefined) {
    return cached;
  }
  const base = DOMSerializer.fromSchema(schema);
  const image = base.nodes.image;
  const serializer =
    image === undefined
      ? base
      : new DOMSerializer(
          {
            ...base.nodes,
            image: (node) => {
              const src = String(node.attrs.src ?? "");
              const route = src.startsWith("dispatch://") ? parseDispatchReference(src) : undefined;
              if (route === undefined) {
                return image(node);
              }
              const named = referencedArtifact(route);
              if (named?.version === undefined) {
                return ["a", { href: src }];
              }
              const alt = String(node.attrs.alt ?? "").trim();
              return [
                "a",
                {
                  "data-dispatch-picture": buildDispatchReference(route),
                  "data-picture-caption": alt === "" ? named.slug : alt,
                  href: buildReferencePath(route),
                },
              ];
            },
          },
          base.marks
        );
  serializers.set(schema, serializer);
  return serializer;
}

/** Every picture link `pictureSerializer` wrote into root, with the caption and the reference
 *  its `DispatchPicture` renders. */
export function collectPictureAnchors(root: HTMLElement): PictureAnchor[] {
  const pictures: PictureAnchor[] = [];
  let index = 0;
  for (const anchor of root.querySelectorAll<HTMLAnchorElement>("a[data-dispatch-picture]")) {
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
