import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { DocumentStage } from "./document-stage";

/** One of the two role-document tab panels, Verification and Plan. They differ only in the tab
 *  they name and the component that frames the document; the panel itself, the lazy-mount guard
 *  and the `ArtifactDocument` under it are one thing, written once.
 *
 *  The document is drawn on its own stage (`document-stage.tsx`), and this panel is where the
 *  stage rests while no row of the tree is being edited in it, so the preset's toolbar and a
 *  row's caret both reach the one editor the page has for it. */
export function RoleDocumentPanel({
  Tab,
  activated,
  active,
  artifact,
  isClosed,
  issueKey,
  onToolbarChange,
  onVersionChange,
  showDiff,
  stage,
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
  /** The page's sink for this document's toolbar, by the document's own id, so the preset
   *  whose document this is draws it (R16). */
  onToolbarChange: (artifactId: string) => (toolbar: DocumentToolbar | undefined) => void;
  onVersionChange: (artifact: Artifact, version: number | null) => void;
  /** Which documents the reader asked to see the changes of, by artifact id. */
  showDiff: ReadonlySet<string>;
  /** Where this document's stage rests while no row holds it. */
  stage: (node: HTMLElement | null) => void;
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
            <>
              <div className="flex min-w-0 flex-col" ref={stage} />
              <DocumentStage artifactId={artifact.id}>
                <ArtifactDocument
                  artifact={artifact}
                  isClosed={isClosed}
                  onToolbarChange={onToolbarChange(artifact.id)}
                  owner={{ key: issueKey, kind: "issue" }}
                  onVersionChange={(version) => onVersionChange(artifact, version)}
                  showDiff={showDiff.has(artifact.id)}
                  version={undefined}
                />
              </DocumentStage>
            </>
          )}
        </Tab>
      ) : null}
    </div>
  );
}
