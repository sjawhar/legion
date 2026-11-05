import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";

/** One of the two role-document tab panels, Verification and Plan. They differ only in the tab
 *  they name and the component that frames the document; the panel itself, the lazy-mount guard
 *  and the `ArtifactDocument` under it are one thing, written once. */
export function RoleDocumentPanel({
  Tab,
  activated,
  active,
  artifact,
  isClosed,
  issueKey,
  onVersionChange,
  tab,
}: {
  Tab: (props: {
    artifact: Artifact | undefined;
    children?: ReactNode;
    issueKey: string;
  }) => ReactNode;
  activated: boolean;
  active: boolean;
  artifact: Artifact | undefined;
  isClosed: boolean;
  issueKey: string;
  onVersionChange: (artifact: Artifact, version: number | null) => void;
  tab: "verification" | "plan";
}): ReactNode {
  return (
    <div
      aria-hidden={!active}
      aria-labelledby={`issue-${tab}-tab`}
      hidden={!active}
      id={`issue-${tab}-panel`}
      role="tabpanel"
    >
      {activated ? (
        <Tab artifact={artifact} issueKey={issueKey}>
          {artifact === undefined ? null : (
            <ArtifactDocument
              artifact={artifact}
              isClosed={isClosed}
              owner={{ key: issueKey, kind: "issue" }}
              onVersionChange={(version) => onVersionChange(artifact, version)}
              showDiff={false}
              version={undefined}
            />
          )}
        </Tab>
      ) : null}
    </div>
  );
}
