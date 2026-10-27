import { encodeBlockId, parseBlockFragment } from "@legion/contracts";

import type { DispatchReferenceRoute } from "./routes";

/** A block of a document, as a reference to it names one (`<document>#b-<id>[@v<N>]`): `block.id`
 *  is the block's id as the document writes it, and `block.pin` the version of the document a
 *  pinned reference names, N's decimal digits as written (`parseBlockFragment`), so a pin past
 *  what a number holds exactly is written back unrounded. A route with a block names no version or
 *  item of its own. */
export interface BlockTarget {
  block?: { id: string; pin?: string };
}

/** `route` naming its document's block `id`, pinned to version `pin` when one is given: only a
 *  spec, an issue artifact or a project document, at no version and naming no item, holds blocks
 *  a reference can name; undefined for any other route. Whether text cites the block is the
 *  caller's to ask (`routeInBound`, `target-id.ts`), as it is for every route. */
export function withBlock(
  route: DispatchReferenceRoute | undefined,
  id: string,
  pin?: string
): DispatchReferenceRoute | undefined {
  if (
    route === undefined ||
    (route.kind !== "spec" && route.kind !== "artifact" && route.kind !== "document") ||
    (route.kind !== "spec" && route.version !== undefined) ||
    (route.kind === "document" && route.item !== undefined)
  ) {
    return undefined;
  }
  return { ...route, block: pin === undefined ? { id } : { id, pin } };
}

/** The block, and the pin, a `<document reference>#b-<block id>[@v<N>]` reference names, as the
 *  server's `text.parseBlock` reads it: the fragment as `parseBlockFragment` reads it, on a
 *  document reference that pins no version of its own. Any other fragment names nothing; the
 *  document itself must still be one `withBlock` admits. */
export function parseBlockReference(
  document: string,
  fragment: string
): { block: string; pin?: string } | undefined {
  if (!fragment.startsWith("b-") || document.includes("@")) {
    return undefined;
  }
  return parseBlockFragment(fragment.slice(2));
}

/** The `#b-` fragment every link to a block of a document carries: the id as `encodeBlockId`
 *  writes it, the one encoding the server's `text.EncodeBlockID` shares. */
export function blockFragment(blockId: string): string {
  return `#b-${encodeBlockId(blockId)}`;
}
