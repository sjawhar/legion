import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { DocEditor } from "../doc/DocEditor";

export function ArtifactDocument({
  artifact,
  commentId,
  highlight,
  highlightTerm,
  isClosed,
  issueKey,
  onVersionChange,
  version,
}: {
  artifact: Artifact;
  commentId?: string;
  highlight?: { from: number; to: number };
  highlightTerm?: string;
  isClosed: boolean;
  issueKey: string;
  onVersionChange: (version: number | null) => void;
  version: number | undefined;
}): ReactNode {
  return (
    <DocEditor
      artifact={artifact}
      commentId={commentId}
      highlight={highlight}
      highlightTerm={highlightTerm}
      isClosed={isClosed}
      issueKey={issueKey}
      key={artifact.id}
      onVersionChange={onVersionChange}
      selectedVersion={version ?? null}
    />
  );
}
