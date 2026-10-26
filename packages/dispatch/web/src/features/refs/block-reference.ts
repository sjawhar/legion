import { encodeBlockId, parseBlockFragment } from "@legion/contracts";

import type { DispatchReferenceRoute } from "./routes";

/** A block of a document, as a reference to it names one (`<document>#b-<id>[@v<N>]`): `block` is
 *  the block's id as the document writes it, and `pin` the version of the document a pinned
 *  reference names, N's decimal digits as written (`parseBlockFragment`), so a pin past what a
 *  number holds exactly is written back unrounded. A route with a block names no version or item
 *  of its own; a pin comes only with a block. */
export interface BlockTarget {
  block?: string;
  pin?: string;
}

/** `route` naming its document's block `block`, pinned to version `pin` when one is given: only a
 *  spec, an issue artifact or a project document, at no version and naming no item, holds blocks
 *  a reference can name; undefined for any other route. */
export function withBlock(
  route: DispatchReferenceRoute | undefined,
  block: string,
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
  return pin === undefined ? { ...route, block } : { ...route, block, pin };
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

/** The block of its document a reference names (`#b-`) and the version of the document it pins,
 *  or undefined for a reference to anything else. */
export function blockTarget(
  route: DispatchReferenceRoute
): { block: string; pin?: string } | undefined {
  if (!("block" in route) || route.block === undefined) {
    return undefined;
  }
  return route.pin === undefined ? { block: route.block } : { block: route.block, pin: route.pin };
}

/** The block of its document a reference names (`#b-`), or undefined for a reference to anything
 *  else. */
export function routeBlock(route: DispatchReferenceRoute): string | undefined {
  return blockTarget(route)?.block;
}
