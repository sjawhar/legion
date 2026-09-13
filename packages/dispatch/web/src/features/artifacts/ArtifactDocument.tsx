import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { queryKeys } from "../../api/query-keys";
import type { Artifact } from "../../api/types";
import type { Highlight } from "../doc/highlight";
import { type DocumentToolbar, ProofDocument } from "../doc/ProofDocument";
import type { MarginOwner } from "../margin/useMarginItems";

export function ArtifactDocument({
  artifact,
  askId,
  commentId,
  highlightTerm,
  isClosed,
  owner,
  onToolbarChange,
  onVersionChange,
  showDiff,
  version,
}: {
  artifact: Artifact;
  askId?: string;
  commentId?: string;
  highlightTerm?: string;
  isClosed: boolean;
  owner: MarginOwner;
  showDiff: boolean;
  onToolbarChange?(toolbar: DocumentToolbar | undefined): void;
  onVersionChange: (version: number | null) => void;
  version: number | undefined;
}): ReactNode {
  const user = useQuery({ queryKey: ["whoami"], queryFn: () => api.whoAmI() });
  const comments = useQuery({
    enabled: commentId !== undefined,
    queryKey:
      owner.kind === "issue"
        ? ["comments", owner.key, artifact.id]
        : ["artifact", owner.artifactId, "comments"],
    queryFn: () =>
      owner.kind === "issue"
        ? api.listComments(owner.key, artifact.id)
        : api.listArtifactComments(owner.artifactId),
  });
  const ask = useQuery({
    enabled: askId !== undefined,
    queryKey: queryKeys.ask(askId),
    queryFn: () => {
      if (askId === undefined) {
        throw new Error("Selected ask query requires an ask id.");
      }
      return api.getAsk(askId);
    },
  });
  const selectedItem =
    commentId === undefined ? ask.data?.ask : comments.data?.find((item) => item.id === commentId);
  const anchor = selectedItem?.anchor;
  const highlight: Highlight | undefined =
    selectedItem === undefined ||
    anchor === null ||
    anchor === undefined ||
    (anchor.orphaned && version === undefined) ||
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
      onToolbarChange={onToolbarChange}
      owner={owner}
      showDiff={showDiff}
      key={artifact.id}
      onVersionChange={onVersionChange}
      user={user.data}
      version={version}
    />
  );
}
