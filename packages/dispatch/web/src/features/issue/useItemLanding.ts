import { ID_AMBIGUOUS } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";

import { ApiError, type ErrorCandidate } from "../../api/client";
import type { IDCandidate, IssueDetails } from "../../api/types";
import {
  askQuery,
  commentQuery,
  messageQuery,
  referencesQuery,
  resolvedPrefixItems,
} from "../refs/reference-target";
import {
  buildIssuePath,
  type DispatchReferenceRoute,
  documentItemPath,
  type IssueRoute,
} from "../refs/routes";
import { isIdPrefix, prefixReference, routeWithWholeIds } from "../refs/text-references";

/** The item routes: `/issues/KEY/asks|comments|messages/<id>`. */
export type ItemRoute = Extract<IssueRoute, { id: string }>;

/**
 * Where an item deep link lands.
 *
 * - `redirect` — the item lives beside a document (an anchored comment or ask, or an ask block),
 *   so its link opens that document with the thread in view.
 * - `rewrite` — the address wrote the id short and it named one item: the page replaces the
 *   address with the whole id, and reads the item under that (`useItemLanding`).
 * - `ambiguous` — the address wrote the id short and it names several items, which the server
 *   listed in its refusal; the page shows them for the reader to choose between.
 * - `turn` — the item has no document; the Conversation focuses its turn.
 * - `missing` — the id names nothing openable: the server rejected it as malformed (400) or has
 *   no such item (404). Either way retrying cannot help; the link is dead.
 * - `unavailable` — the lookup itself failed; the reader can retry it.
 */
export type ItemLanding =
  | { kind: "pending" }
  | { kind: "redirect"; to: string }
  | { kind: "rewrite"; to: string }
  | { kind: "ambiguous"; candidates: readonly IDCandidate[] }
  | { kind: "turn" }
  | { kind: "missing" }
  | { kind: "unavailable"; retry: () => void };

export function isItemRoute(route: IssueRoute): route is ItemRoute {
  return route.kind === "ask" || route.kind === "comment" || route.kind === "message";
}

/** The items an `ID_AMBIGUOUS` refusal listed, or undefined for any other failure and for a
 *  refusal that listed none, which there is no list to show for. The code is what says the
 *  entries are these and not a duplicate check's issues (`ErrorCandidate`). */
export function ambiguousItems(error: unknown): readonly IDCandidate[] | undefined {
  if (!(error instanceof ApiError) || error.code !== ID_AMBIGUOUS) {
    return undefined;
  }
  const items = error.candidates?.filter(isIDCandidate) ?? [];
  return items.length === 0 ? undefined : items;
}

function isIDCandidate(candidate: ErrorCandidate): candidate is IDCandidate {
  return "id" in candidate && "kind" in candidate;
}

/**
 * Resolves an item deep link against the item itself. `undefined` for every other issue route.
 * `issue` supplies the artifacts an anchor names; until it arrives the landing stays pending, so
 * an anchored item never flashes its Conversation turn before opening its document.
 *
 * An address whose id is a whole uuid is read exactly as it always was: the item's own read
 * under that id, and nothing else. Only an id written short (`isIdPrefix`) is resolved first,
 * through the graph read of the reference the address names, which the server resolves within
 * the issue that address names; the item is then read under the whole id the answer carries, so
 * nothing is ever read or cached under a prefix. One match replaces the address with the whole
 * id (`rewrite`), a prefix the server refused as ambiguous lists its matches, and one that names
 * nothing is the 404 a whole id that names nothing already is.
 */
export function useItemLanding(
  route: IssueRoute,
  issue: IssueDetails | undefined
): ItemLanding | undefined {
  const origin = window.location.origin;
  const address = isItemRoute(route) ? route : undefined;
  const prefix =
    address !== undefined && isIdPrefix(address.id) ? prefixReference(address) : undefined;
  const resolution = useQuery({ ...referencesQuery(prefix), enabled: prefix !== undefined });
  // The route the address names with its id written whole: the address's own route where it
  // wrote it whole, and the graph read's answer where it wrote it short. An answer naming a
  // record of another kind, or none, leaves it undefined, and the landing below is the 404.
  const answered: DispatchReferenceRoute | undefined =
    prefix === undefined || address === undefined
      ? address
      : routeWithWholeIds(address, resolvedPrefixItems(resolution.data), origin);
  const whole =
    answered !== undefined && "key" in answered && isItemRoute(answered) ? answered : undefined;
  // The item's own read waits for the whole id: a prefix is never a key anything is cached
  // under.
  const id = whole?.id;
  const comment = useQuery({
    ...commentQuery(id),
    enabled: route.kind === "comment" && id !== undefined,
  });
  const ask = useQuery({ ...askQuery(id), enabled: route.kind === "ask" && id !== undefined });
  const message = useQuery({
    ...messageQuery(route.key, id),
    enabled: route.kind === "message" && id !== undefined,
  });

  if (!isItemRoute(route)) {
    return undefined;
  }
  if (prefix !== undefined) {
    if (resolution.isPending) {
      return { kind: "pending" };
    }
    if (resolution.isError) {
      const candidates = ambiguousItems(resolution.error);
      if (candidates !== undefined) {
        return { candidates, kind: "ambiguous" };
      }
      const dead =
        resolution.error instanceof ApiError &&
        (resolution.error.status === 404 || resolution.error.status === 400);
      return dead
        ? { kind: "missing" }
        : { kind: "unavailable", retry: () => void resolution.refetch() };
    }
    // The answer named one item: the address is rewritten to its whole id, and the page lands
    // again on the route that id makes, which reads the item as any whole-id address does.
    return whole === undefined
      ? { kind: "missing" }
      : { kind: "rewrite", to: buildIssuePath(whole) };
  }
  // One decision per route kind: which query answers it, the item it returns, and the document
  // that item belongs beside.
  const resolved =
    route.kind === "comment"
      ? {
          artifactID: comment.data?.comment.anchor?.artifact_id,
          blockID: undefined,
          item: comment.data?.comment,
          query: comment,
        }
      : route.kind === "ask"
        ? {
            // An approval ask carries no anchor or block of its own, but it names the document
            // it asks about (`approval.artifact_id`); landing there, with the ask in the
            // margin, beats falling through to the Conversation turn below.
            artifactID:
              ask.data?.ask.anchor?.artifact_id ??
              ask.data?.ask.block_artifact?.id ??
              ask.data?.ask.approval?.artifact_id,
            blockID: ask.data?.ask.block_id ?? undefined,
            item: ask.data?.ask,
            query: ask,
          }
        : {
            artifactID: undefined,
            blockID: undefined,
            item: message.data?.message,
            query: message,
          };
  const { artifactID, blockID, item, query } = resolved;
  if (query.isPending) {
    return { kind: "pending" };
  }
  if (query.isError) {
    const dead =
      query.error instanceof ApiError && (query.error.status === 404 || query.error.status === 400);
    return dead ? { kind: "missing" } : { kind: "unavailable", retry: () => void query.refetch() };
  }

  if (item === undefined) {
    return { kind: "missing" };
  }
  if (item.issue_key !== route.key) {
    // A pasted or stale link under the wrong issue: send the reader to the issue that owns the
    // item, which resolves the item again from there. An item with no issue is not reachable
    // under an issue route at all.
    return item.issue_key === null
      ? { kind: "missing" }
      : { kind: "redirect", to: buildIssuePath({ ...route, key: item.issue_key }) };
  }
  if (route.kind === "message") {
    return { kind: "turn" };
  }

  if (artifactID === undefined) {
    return { kind: "turn" };
  }
  if (issue === undefined) {
    return { kind: "pending" };
  }
  const artifact = issue.artifacts.find((candidate) => candidate.id === artifactID);
  if (artifact === undefined || artifact.kind !== "doc") {
    return { kind: "turn" };
  }
  return {
    kind: "redirect",
    to: documentItemPath(artifact, { blockID, id: route.id, kind: route.kind }),
  };
}
