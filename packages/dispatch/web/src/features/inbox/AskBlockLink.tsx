import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import { blockFragment } from "../refs/block-reference";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { isDecisionAsk } from "./ask-decision";

export function AskBlockLink({ ask }: { ask: Ask }): ReactNode {
  if (!isDecisionAsk(ask)) {
    return null;
  }
  const fragment = blockFragment(ask.block_id);
  if (ask.issue_key !== null) {
    const route = ask.block_artifact.primary
      ? buildIssuePath({ key: ask.issue_key, kind: "spec" })
      : buildIssuePath({ key: ask.issue_key, kind: "artifact", slug: ask.block_artifact.slug });
    return <Link to={`${route}${fragment}`}>Open in document</Link>;
  }
  if (ask.document !== undefined) {
    return (
      <Link
        to={`${buildProjectPath({ kind: "document", project: ask.document.project, slug: ask.document.slug })}${fragment}`}
      >
        Open in document
      </Link>
    );
  }
  return null;
}
