import { lazy, type ReactNode, Suspense } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { dangerText, linkHoverText, linkText } from "../../theme/classes";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath, type DispatchRoute } from "../refs/routes";
import { ArtifactBlobView, ArtifactHeader } from "./ArtifactHeader";
import { ArtifactsTab } from "./ArtifactsTab";

/**
 * The artifact's details - its name, who wrote it, its versions - which stand under the document
 * rather than over it.
 *
 * In a chunk and a `Suspense` boundary of its own, so the document's own part of the issue page
 * commits without them and they land in a commit after it (`IssuePage.tsx`,
 * dispatch://INFRA-341 R11). Nothing above them moves when they do, and the chunk is downloaded
 * from the address beside the page's own (`DOCUMENT_ADDRESS_MODULES` in `web/vite.config.ts`).
 */
const ArtifactDetails = lazy(() =>
  import("./ArtifactDetails").then((module) => ({ default: module.ArtifactDetails }))
);

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
          <ArtifactsTab isClosed={isClosed} />
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
            <div className="space-y-4">
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
              <Suspense fallback={null}>
                <ArtifactDetails artifact={artifact} />
              </Suspense>
            </div>
          )
        ) : null
      ) : null}
    </div>
  );
}
