import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";

/**
 * Every anchored ask on an issue's visible artifact - open, answered, or resolved - ordered
 * by where it anchors in the document.
 *
 * The server, not this tab's own history, is authoritative: a single per-issue query (not a
 * fan-out over individually observed IDs) means a viewer who opens the issue only after an ask
 * is closed sees it too, exactly like one who watched it close live.
 */
export function useAnsweredAsks(issueKey: string | undefined, artifactID: string | undefined) {
  const query = useQuery({
    enabled: issueKey !== undefined,
    queryFn: () => api.listIssueAsks(issueKey ?? "", "all"),
    queryKey: ["asks", issueKey],
  });
  const asks = useMemo(
    () =>
      (query.data ?? [])
        .filter((ask) => ask.anchor?.artifact_id === artifactID)
        .sort(
          (left, right) =>
            (left.anchor?.from ?? 0) - (right.anchor?.from ?? 0) ||
            (left.anchor?.to ?? 0) - (right.anchor?.to ?? 0) ||
            left.id.localeCompare(right.id)
        ),
    [artifactID, query.data]
  );

  return {
    asks,
    error: query.isError ? query : undefined,
    pending: query.isPending,
  };
}
