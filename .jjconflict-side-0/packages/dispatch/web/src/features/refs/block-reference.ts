import { encodeBlockId, parseBlockFragment } from "@legion/contracts";

import type { DispatchReferenceRoute } from "./routes";

/** A block, as a reference to it names one (`<document or item>#b-<id>[@v<N>]`): `block.id` is
 *  the block's id as its document or its comment or message writes it, and `block.pin` the
 *  version of the document, or the revision of the comment, a pinned reference names, N's decimal
 *  digits as written (`parseBlockFragment`), so a pin past what a number holds exactly is written
 *  back unrounded. A route with a block names no version of its own, and no item but the comment
 *  whose block it names. */
export interface BlockTarget {
  block?: { id: string; pin?: string };
}

/** The comment or issue message whose block `route` names, or undefined when it names a block of
 *  a document or no block: an issue's comment or message, or a project document's comment. */
export function itemOfBlock(
  route: DispatchReferenceRoute
): { kind: "comment" | "message"; id: string } | undefined {
  if (!("block" in route) || route.block === undefined) {
    return undefined;
  }
  if (route.kind === "comment" || route.kind === "message") {
    return { id: route.id, kind: route.kind };
  }
  return route.kind === "document" && route.item?.kind === "comment"
    ? { id: route.item.id, kind: "comment" }
    : undefined;
}

/** `route` naming its block `id`, pinned to `pin` when one is given, as the server's
 *  `text.parseBlock` admits one: a block of a spec, an issue artifact or a project document at no
 *  version, pinned to a version of the document; a block of a comment, an issue's or a project
 *  document's (a comment's blocks are its own, so the document's version is dropped), pinned to a
 *  revision of the comment; and an unpinned block of an issue message, which never changes.
 *  Undefined for any other route. Whether text cites the block is the caller's to ask
 *  (`routeInBound`, `target-id.ts`), as it is for every route. */
export function withBlock(
  route: DispatchReferenceRoute | undefined,
  id: string,
  pin?: string
): DispatchReferenceRoute | undefined {
  const block = pin === undefined ? { id } : { id, pin };
  switch (route?.kind) {
    case "spec":
    case "comment":
      return { ...route, block };
    case "message":
      return pin === undefined ? { ...route, block } : undefined;
    case "artifact":
      return route.version === undefined ? { ...route, block } : undefined;
    case "document":
      if (route.item === undefined) {
        return route.version === undefined ? { ...route, block } : undefined;
      }
      return route.item.kind === "comment"
        ? { block, item: route.item, kind: "document", project: route.project, slug: route.slug }
        : undefined;
    default:
      return undefined;
  }
}

/** The block, and the pin, a `<document or item reference>#b-<block id>[@v<N>]` reference names,
 *  as the server's `text.parseBlock` reads it: the fragment as `parseBlockFragment` reads it, on a
 *  reference that pins no version of its own. Any other fragment names nothing; the document or
 *  item itself must still be one `withBlock` admits. */
export function parseBlockReference(
  document: string,
  fragment: string
): { block: string; pin?: string } | undefined {
  if (!fragment.startsWith("b-") || document.includes("@")) {
    return undefined;
  }
  return parseBlockFragment(fragment.slice(2));
}

/** The `#b-` fragment every link to a block of a document, a comment or a message carries: the id
 *  as `encodeBlockId` writes it, the one encoding the server's `text.EncodeBlockID` shares. */
export function blockFragment(blockId: string): string {
  return `#b-${encodeBlockId(blockId)}`;
}
