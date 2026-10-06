import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import { documentItemPath } from "../refs/routes";

/**
 * The document an approval ask asks about, as a link naming both the document and the version it
 * asked (`approval.version`, kept current while the request is open — see `AskApproval`). Built
 * through `documentItemPath`, the same shape every in-app thread link uses: it opens the
 * document's current route, never a historical `@vN` snapshot, with this ask selected in the
 * margin. A reader who follows it after the document has moved further still reaches the live
 * document, with the approve controls in view, and the text beside it keeps saying which version
 * the request named.
 */
export function AskApprovalLink({ ask }: { ask: Ask }): ReactNode {
  const { approval, approval_artifact: artifact } = ask;
  if (approval === undefined || artifact === undefined) {
    return null;
  }
  const route = documentItemPath(
    {
      issue_key: ask.issue_key,
      kind: "doc",
      primary: artifact.primary,
      project: artifact.project,
      slug: artifact.slug,
    },
    { id: ask.id, kind: "ask" }
  );
  return (
    <Link className="underline" to={route}>
      {approval.name}, version {approval.version}
    </Link>
  );
}
