import { lazy, type ReactNode, Suspense } from "react";

import type { Artifact } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { isAgentArtifact } from "../refs/routes";
import { versionsNewestFirst } from "./ArtifactHeader";

/** The panel itself, which only a page file shows: the rows, the form and the two writes are a
 *  chunk of their own, so every other document's reader fetches none of them. */
const ArtifactGrants = lazy(() =>
  import("./ArtifactGrants").then((module) => ({ default: module.ArtifactGrants }))
);

/**
 * Whether this file is a page file a `view` block can show, read as the server reads it
 * (`isPageFile` in packages/envoy/internal/dispatch/api/artifacts.go): a file whose stored type
 * is `text/html`, or whose name ends `.html` or `.htm`. The type is the one the version on
 * screen was stored under - the version the route names, else the newest, as `ArtifactBlobView`
 * picks the version whose size it shows. A document is never a page file, because its text is
 * markdown Dispatch rewrites rather than the bytes an author sent, and nor is a file an agent's
 * conversation owns, which belongs to neither an issue nor a project and whose uploads announce
 * nothing a page could redraw from. The grants routes refuse every other file `415 NOT_A_PAGE`,
 * so the panel asks for no list it could not get.
 */
function isPageFile(artifact: Artifact, version: number | undefined): boolean {
  if (artifact.kind === "doc" || isAgentArtifact(artifact)) {
    return false;
  }
  const versions = versionsNewestFirst(artifact.versions);
  const selected =
    version === undefined ? versions[0] : versions.find((item) => item.number === version);
  const mime = selected?.mime?.split(";")[0]?.trim().toLowerCase();
  if (mime === "text/html") {
    return true;
  }
  const name = artifact.name.toLowerCase();
  return name.endsWith(".html") || name.endsWith(".htm");
}

/**
 * The gate in front of "Data this page may read" (`ArtifactGrants`): a person alone writes a
 * grant (`useIsHuman`), and only an issue's or a project's page file has any, so this is all a
 * document's reader loads of the panel.
 */
export function ArtifactGrantsPanel({
  artifact,
  version,
}: {
  artifact: Artifact;
  version: number | undefined;
}): ReactNode {
  const human = useIsHuman();
  if (!human || !isPageFile(artifact, version)) {
    return null;
  }
  return (
    <Suspense fallback={null}>
      <ArtifactGrants artifact={artifact} />
    </Suspense>
  );
}
