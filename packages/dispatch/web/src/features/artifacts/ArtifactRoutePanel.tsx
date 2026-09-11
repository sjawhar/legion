import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { dangerText, linkHoverText, linkText } from "../../theme/classes";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath, type DispatchRoute } from "../refs/routes";
import { ArtifactBlobView, ArtifactHeader } from "./ArtifactHeader";
import { ArtifactsTab } from "./ArtifactsTab";

export function ArtifactRoutePanel({
  active,
  artifact,
  artifactRoute,
  children,
  isClosed,
  isPrimaryArtifactRoute,
  mounted,
  onShowDiffChange,
  route,
  showDiff,
  toolbar,
}: {
  active: boolean;
  artifact: Artifact | undefined;
  artifactRoute: Extract<DispatchRoute, { kind: "artifact" }> | undefined;
  children: ReactNode;
  isClosed: boolean;
  isPrimaryArtifactRoute: boolean;
  mounted: boolean;
  onShowDiffChange(next: boolean): void;
  route: DispatchRoute;
  showDiff: boolean;
  toolbar: DocumentToolbar | undefined;
}): ReactNode {
  return (
    <div
      aria-hidden={!active}
      aria-labelledby="issue-artifacts-tab"
      hidden={!active}
      id="issue-artifacts-panel"
      role="tabpanel"
    >
      {mounted ? (
        route.kind === "artifacts" ? (
          <ArtifactsTab />
        ) : artifactRoute !== undefined && !isPrimaryArtifactRoute ? (
          artifact === undefined ? (
            <section className="space-y-3">
              <p className={dangerText}>
                No artifact {artifactRoute.slug} on {artifactRoute.key}.
              </p>
              <Link
                className={`font-medium underline ${linkText} ${linkHoverText}`}
                to={buildIssuePath({ key: artifactRoute.key, kind: "artifacts" })}
              >
                View artifacts
              </Link>
            </section>
          ) : (
            <ArtifactHeader
              artifact={artifact}
              highlight
              isClosed={isClosed}
              onShowDiffChange={onShowDiffChange}
              showDiff={showDiff}
              showVersionPicker
              toolbar={artifact.kind === "doc" ? toolbar : undefined}
              version={artifactRoute.version}
            >
              {artifact.kind === "doc" ? (
                children
              ) : (
                <ArtifactBlobView artifact={artifact} version={artifactRoute.version} />
              )}
            </ArtifactHeader>
          )
        ) : null
      ) : null}
    </div>
  );
}
