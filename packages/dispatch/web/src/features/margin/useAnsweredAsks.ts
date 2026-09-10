import { useQueries } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { api } from "../../api/client";
import type { Ask } from "../../api/types";

/**
 * Merges live anchored inbox asks with answered asks that have left the open-only inbox feed.
 *
 * Once an anchored ask is observed for an issue/artifact, its ID stays tracked for that view.
 * When the inbox drops it after an answer, fetching its authoritative record keeps the answered
 * detail visible for every viewer, including viewers who did not submit that answer.
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
      queryKey: ["ask", id],
      queryFn: () => api.getAsk(id),
    })),
  });
  // useQueries returns a fresh array identity every render even when nothing changed; key the
  // memo on answered IDs so consumers do not recompute and re-fire their effects forever.
  const answeredMissingAskIds = missingAskQueries
    .map((query) => (query.data?.state === "answered" ? query.data.id : undefined))
    .filter((id): id is string => id !== undefined)
    .join(",");
  // biome-ignore lint/correctness/useExhaustiveDependencies: keyed on answered IDs, not unstable useQueries array
  const answeredMissingAsks = useMemo(
    () =>
      missingAskQueries
        .map((query) => query.data)
        .filter((ask): ask is Ask => ask !== undefined && ask.state === "answered"),
    [answeredMissingAskIds]
  );

  const mergedAsks = useMemo(
    () => [...liveAnchoredAsks, ...answeredMissingAsks],
    [liveAnchoredAsks, answeredMissingAsks]
  );

  return {
    asks: mergedAsks,
    error: missingAskQueries.find((query) => query.isError),
    pending: missingAskQueries.some((query) => query.isPending),
  };
}
