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
 * Whether this file is a page file a `view` block can show, as the server tests it (`isPageFile`
 * in packages/envoy/internal/dispatch/api/artifacts.go): an issue's or a project's file whose
 * **latest** version's stored type is `text/html`, or whose name ends `.html` or `.htm`.
 *
 * The test is over the file and never over the version on screen, because that is the question
 * the three routes answer: a grant hangs from the file and covers every version of it, the
 * panel lists a file's grants with no version in hand, and the grants routes answer
 * `415 NOT_A_PAGE` from the latest version's type whichever version a reader is looking at. A
 * panel shown on an older HTML version of a file that is no longer a page would ask for a list
 * the server refuses; a panel hidden on an older non-HTML version of a file that is one now
 * would hide the grants a running block rests on.
 *
 * A document is never a page file, because its text is markdown Dispatch rewrites rather than
 * the bytes an author sent, and nor is a file an agent's conversation owns, which belongs to
 * neither an issue nor a project and whose uploads announce nothing a page could redraw from.
 */
function isPageFile(artifact: Artifact): boolean {
  if (artifact.kind === "doc" || isAgentArtifact(artifact)) {
    return false;
  }
  const latest = versionsNewestFirst(artifact.versions)[0];
  const mime = latest?.mime?.split(";")[0]?.trim().toLowerCase();
  if (mime === "text/html") {
    return true;
  }
  const name = artifact.name.toLowerCase();
  return name.endsWith(".html") || name.endsWith(".htm");
}

/**
 * The gate in front of "Data this page may read" (`ArtifactGrants`): a person alone writes a
 * grant (`useIsHuman`), and only an issue's or a project's page file has any, so this is all a
 * document's reader loads of the panel. The version a reader is looking at does not enter into
 * it: the grants are the file's, as the server's own page test is (`isPageFile`).
 */
export function ArtifactGrantsPanel({ artifact }: { artifact: Artifact }): ReactNode {
  const human = useIsHuman();
  if (!human || !isPageFile(artifact)) {
    return null;
  }
  return (
    <Suspense fallback={null}>
      <ArtifactGrants artifact={artifact} />
    </Suspense>
  );
}
