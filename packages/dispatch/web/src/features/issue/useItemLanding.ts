import { useQuery } from "@tanstack/react-query";

import { ApiError } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { buildIssuePath, documentItemPath, type IssueRoute } from "../refs/routes";
import { askQuery, commentQuery, messageQuery } from "../refs/Unfurl";

/** The item routes: `/issues/KEY/asks|comments|messages/<id>`. */
export type ItemRoute = Extract<IssueRoute, { id: string }>;

/**
 * Where an item deep link lands.
 *
 * - `redirect` — the item lives beside a document (an anchored comment or ask, or an ask block),
 *   so its link opens that document with the thread in view.
 * - `turn` — the item has no document; the Conversation focuses its turn.
 * - `missing` — the id names nothing openable: the server rejected it as malformed (400) or has
 *   no such item (404). Either way retrying cannot help; the link is dead.
 * - `unavailable` — the lookup itself failed; the reader can retry it.
 */
export type ItemLanding =
  | { kind: "pending" }
  | { kind: "redirect"; to: string }
  | { kind: "turn" }
  | { kind: "missing" }
  | { kind: "unavailable"; retry: () => void };

export function isItemRoute(route: IssueRoute): route is ItemRoute {
  return route.kind === "ask" || route.kind === "comment" || route.kind === "message";
}

/**
 * Resolves an item deep link against the item itself. `undefined` for every other issue route.
 * `issue` supplies the artifacts an anchor names; until it arrives the landing stays pending, so
 * an anchored item never flashes its Conversation turn before opening its document.
 */
export function useItemLanding(
  route: IssueRoute,
  issue: IssueDetails | undefined
): ItemLanding | undefined {
  const id = isItemRoute(route) ? route.id : undefined;
  const comment = useQuery({ ...commentQuery(id), enabled: route.kind === "comment" });
  const ask = useQuery({ ...askQuery(id), enabled: route.kind === "ask" });
  const message = useQuery({ ...messageQuery(route.key, id), enabled: route.kind === "message" });

  if (!isItemRoute(route)) {
    return undefined;
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
            artifactID: ask.data?.ask.anchor?.artifact_id ?? ask.data?.ask.block_artifact?.id,
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
