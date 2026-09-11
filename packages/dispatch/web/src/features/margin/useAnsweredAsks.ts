import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import type { MarginOwner } from "./useMarginItems";

/**
 * Every anchored ask on the visible artifact. Document-owned asks arrive from the artifact
 * endpoint; issue-owned asks retain the single per-issue history query.
 */
export function useAnsweredAsks(owner: MarginOwner | undefined, artifactID: string | undefined) {
  const query = useQuery({
    enabled: owner !== undefined,
    queryFn: () => {
      if (owner === undefined) {
        throw new Error("Answered asks require a margin owner.");
      }
      return owner.kind === "document"
        ? api.listArtifactAsks(owner.artifactId, "all")
        : api.listIssueAsks(owner.key, "all");
    },
    queryKey:
      owner?.kind === "document"
        ? ["artifact", owner.artifactId, "asks"]
        : ["asks", owner?.kind === "issue" ? owner.key : undefined],
  });
  const asks = useMemo(
    () =>
      owner?.kind === "document"
        ? (query.data ?? [])
        : (query.data ?? []).filter((ask) => ask.anchor?.artifact_id === artifactID),
    [artifactID, owner?.kind, query.data]
  );

  return {
    asks,
    error: query.isError ? query : undefined,
    pending: query.isPending,
  };
}
