import { MAX_BLOCK_ID_BYTES, referenceTargetFits, utf8Bytes } from "@legion/contracts";

import type { DispatchReferenceRoute } from "./routes";

/**
 * The node of the server's reference graph a route names - its kind and its target id, Go's
 * `Ref.Kind` and `text.TargetID` - and whether text citing the route cites that node, the bounds
 * the graph's index keys. The readers of text (`parseDispatchReference`,
 * `referenceRouteFromHref`) hold a route to them; the readers of what the server returned
 * (`parseNodeReference`, `nodeRouteFromHref`) do not.
 */

/** What a reference resolves to for display: the record whose title, excerpt, or author the
 * reader is shown. An issue's spec/log/activity/children/artifacts tabs all resolve to the issue
 * itself; an ask or comment nested under a project document resolves to that item, not the
 * document; a block of a document (`#b-`) resolves to that block. */
export type ReferenceTargetKind = "issue" | "document" | "block" | "ask" | "comment" | "message";

export function referenceTargetKind(route: DispatchReferenceRoute): ReferenceTargetKind {
  if ("block" in route && route.block !== undefined) {
    return "block";
  }
  if (route.kind === "document") {
    return route.item?.kind ?? "document";
  }
  switch (route.kind) {
    case "ask":
    case "comment":
    case "message":
      return route.kind;
    case "artifact":
    case "agent-artifact":
      return "document";
    default:
      return "issue";
  }
}

/** The target id the server's graph stores a reference to `route` under, as Go's `text.TargetID`
 *  writes it: an ask's, a comment's or a message's id, a document's `<key or project>/<slug>`
 *  (`spec` for an issue's spec) or `agent/<session id>/<slug>`, a block's `#<block id>` after its
 *  document's, and an issue's key for the issue and each of its tabs. A version or a pin is no
 *  part of it. */
function routeTarget(route: DispatchReferenceRoute): string {
  const block = "block" in route && route.block !== undefined ? `#${route.block.id}` : "";
  switch (route.kind) {
    case "agent-artifact":
      return `agent/${route.session}/${route.slug}`;
    case "document":
      return route.item?.id ?? `${route.project}/${route.slug}${block}`;
    case "spec":
      return `${route.key}/spec${block}`;
    case "artifact":
      return `${route.key}/${route.slug}${block}`;
    case "ask":
    case "comment":
    case "message":
      return route.id;
    default:
      return route.key;
  }
}

/** Whether text citing `route` cites it, as the server's `text.Extract` reads text (Go's
 *  `inBound`): its target id fits (`referenceTargetFits` of `routeTarget`), and a block's id holds
 *  at most MAX_BLOCK_ID_BYTES. Those bounds decide only what the graph indexes: the reference a
 *  node the server returned carries, or a page it links to, is read whatever its length
 *  (`parseNodeReference`, `nodeRouteFromHref`). */
export function routeInBound(route: DispatchReferenceRoute): boolean {
  return (
    (!("block" in route) ||
      route.block === undefined ||
      utf8Bytes(route.block.id) <= MAX_BLOCK_ID_BYTES) &&
    referenceTargetFits(routeTarget(route))
  );
}

/** What a dashboard page cites in text, as the server's `pageInBound` reads it: `block`, the block
 *  its fragment names, while it is in bound (`routeInBound`), else `page`, what the page itself
 *  names, while that is, else nothing. */
export function pageInBound(
  page: DispatchReferenceRoute,
  block: DispatchReferenceRoute | undefined
): DispatchReferenceRoute | undefined {
  if (block !== undefined && routeInBound(block)) {
    return block;
  }
  return routeInBound(page) ? page : undefined;
}
