import type { ReactNode } from "react";

import type { Artifact, AuthenticatedUser } from "../../api/types";
import { DocEditor } from "../doc/DocEditor";

export function ArtifactDocument({
  artifact,
  highlight,
  isClosed,
  issueKey,
  onVersionChange,
  user,
  version,
}: {
  artifact: Artifact;
  highlight: { from: number; to: number } | undefined;
  isClosed: boolean;
  issueKey: string;
  onVersionChange: (version: number | null) => void;
  user: AuthenticatedUser;
  version: number | undefined;
}): ReactNode {
  return (
    <DocEditor
      artifact={artifact}
      highlight={highlight}
      isClosed={isClosed}
      issueKey={issueKey}
      key={artifact.id}
      onVersionChange={onVersionChange}
      selectedVersion={version ?? null}
      user={user}
    />
  );
}
