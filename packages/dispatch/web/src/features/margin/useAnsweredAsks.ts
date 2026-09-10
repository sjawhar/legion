import { useQueries } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { api } from "../../api/client";
import type { Ask } from "../../api/types";

/**
 * Merges live anchored inbox asks with closed asks that have left the open-only inbox feed.
 *
 * Once an anchored ask is observed for an issue/artifact, its ID stays tracked for that view.
 * When the inbox drops it after an answer or resolution, fetching its authoritative record keeps
 * the closed detail visible for every viewer, including viewers who did not close it.
 */
export function useAnsweredAsks(
  asks: Ask[] | undefined,
  issueKey: string | undefined,
  artifactID: string | undefined
) {
  const liveAnchoredAsks = useMemo(
    () =>
      (asks ?? []).filter(
        (ask) => ask.issue_key === issueKey && ask.anchor?.artifact_id === artifactID
      ),
    [asks, issueKey, artifactID]
  );
  const [seenAskIds, setSeenAskIds] = useState<ReadonlySet<string>>(new Set());

  // biome-ignore lint/correctness/useExhaustiveDependencies: issueKey/artifact gate the reset, not the body
  useEffect(() => {
    setSeenAskIds(new Set());
  }, [issueKey, artifactID]);
  useEffect(() => {
    setSeenAskIds((current) => {
      const next = new Set(current);
      let changed = false;
      for (const ask of liveAnchoredAsks) {
        if (!next.has(ask.id)) {
          next.add(ask.id);
          changed = true;
        }
      }
      return changed ? next : current;
    });
  }, [liveAnchoredAsks]);

  const missingAskIds = useMemo(
    () => [...seenAskIds].filter((id) => !liveAnchoredAsks.some((ask) => ask.id === id)),
    [seenAskIds, liveAnchoredAsks]
  );
  const missingAskQueries = useQueries({
    queries: missingAskIds.map((id) => ({
      // GET /asks/:id returns {ask, replies} (AskCard renders the reply thread too);
      // this hook only needs the ask itself.
      queryFn: async () => (await api.getAsk(id)).ask,
      queryKey: ["ask", id],
    })),
  });
  // useQueries returns a fresh array identity every render even when nothing changed; key the
  // memo on closed IDs so consumers do not recompute and re-fire their effects forever.
  const closedMissingAskIds = missingAskQueries
    .map((query) => (query.data?.state !== "open" ? query.data?.id : undefined))
    .filter((id): id is string => id !== undefined)
    .join(",");
  // biome-ignore lint/correctness/useExhaustiveDependencies: keyed on closed IDs, not unstable useQueries array
  const closedMissingAsks = useMemo(
    () =>
      missingAskQueries
        .map((query) => query.data)
        .filter((ask): ask is Ask => ask !== undefined && ask.state !== "open"),
    [closedMissingAskIds]
  );

  const mergedAsks = useMemo(
    () => [...liveAnchoredAsks, ...closedMissingAsks],
    [liveAnchoredAsks, closedMissingAsks]
  );

  return {
    asks: mergedAsks,
    error: missingAskQueries.find((query) => query.isError),
    pending: missingAskQueries.some((query) => query.isPending),
  };
}
