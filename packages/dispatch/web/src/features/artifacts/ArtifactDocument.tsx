import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import type { Highlight } from "../doc/highlight";
import { ProofDocument } from "../doc/ProofDocument";

export function ArtifactDocument({
  artifact,
  askId,
  commentId,
  highlightTerm,
  isClosed,
  issueKey,
  onVersionChange,
  version,
}: {
  artifact: Artifact;
  askId?: string;
  commentId?: string;
  highlightTerm?: string;
  isClosed: boolean;
  issueKey: string;
  onVersionChange: (version: number | null) => void;
  version: number | undefined;
}): ReactNode {
  const user = useQuery({ queryKey: ["whoami"], queryFn: () => api.whoAmI() });
  const comments = useQuery({
    enabled: commentId !== undefined,
    queryKey: ["comments", issueKey, artifact.id],
    queryFn: () => api.listComments(issueKey, artifact.id),
  });
  const ask = useQuery({
    enabled: askId !== undefined,
    queryKey: ["ask", askId],
    queryFn: () => api.getAsk(askId ?? ""),
  });
  const selectedItem =
    commentId === undefined ? ask.data?.ask : comments.data?.find((item) => item.id === commentId);
  const anchor = selectedItem?.anchor;
  const highlight: Highlight | undefined =
    selectedItem === undefined ||
    anchor === null ||
    anchor === undefined ||
    anchor.orphaned ||
    anchor.artifact_id !== artifact.id
      ? undefined
      : {
          by: `${selectedItem.author.kind}:${selectedItem.author.id}`,
          id: selectedItem.id,
          quote: anchor.quote,
        };

  if (user.data === undefined) {
    return <p>Loading document…</p>;
  }

  return (
    <ProofDocument
      artifact={artifact}
      highlight={highlight}
      highlightTerm={highlightTerm}
      isClosed={isClosed}
      issueKey={issueKey}
      key={artifact.id}
      onVersionChange={onVersionChange}
      user={user.data}
      version={version}
    />
  );
}
