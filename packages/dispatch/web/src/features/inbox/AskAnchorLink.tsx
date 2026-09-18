import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import { buildIssuePath, buildProjectPath } from "../refs/routes";

/** The document named by an ask's quote anchor, including its stable block fragment when known. */
export function AskAnchorLink({ ask }: { ask: Ask }): ReactNode {
  const { anchor, anchor_artifact: artifact } = ask;
  if (anchor === null || artifact === undefined) {
    return null;
  }
  const fragment =
    anchor.block_id === undefined || anchor.block_id === null
      ? ""
      : `#b-${encodeURIComponent(anchor.block_id)}`;
  if (ask.issue_key !== null) {
    const route = artifact.primary
      ? buildIssuePath({ key: ask.issue_key, kind: "spec" })
      : buildIssuePath({ key: ask.issue_key, kind: "artifact", slug: artifact.slug });
    return (
      <Link className="underline" to={`${route}${fragment}`}>
        {artifact.name}
      </Link>
    );
  }
  return (
    <Link
      className="underline"
      to={`${buildProjectPath({ kind: "document", project: artifact.project, slug: artifact.slug })}${fragment}`}
    >
      {artifact.name}
    </Link>
  );
}
