import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueComponents } from "../../api/types";
import {
  badgeHigh,
  badgePrimary,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";

/**
 * The `Components:` line of the issue header's metadata rail: the issue's effective
 * attachment as read-only chips. An explicit set lists its live component ids and marks each
 * id a re-import retired; `none` shows the reason; an issue no ancestor chain attached reads
 * `Not attached`. When the attachment is an ancestor's, that ancestor is named and linked.
 * Editing the attachment is the component tree's picker, not this line.
 */
export function IssueComponentsLine({ components }: { components: IssueComponents }): ReactNode {
  const inheritedFrom =
    components.inherited_from === null ? null : (
      <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>
        inherited from{" "}
        <Link
          className={`underline ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: components.inherited_from, kind: "issue" })}
        >
          {components.inherited_from}
        </Link>
      </span>
    );
  return (
    <div
      className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}
      data-testid="issue-components"
    >
      <span className="font-medium">Components:</span>
      {components.mode === "inherit" ? (
        <span className={textMutedOnSurface}>Not attached</span>
      ) : components.mode === "none" ? (
        <span className={textMutedOnSurface}>None — {components.reason}</span>
      ) : (
        <>
          {components.ids.map((id) => (
            <span
              className={`inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium ${badgePrimary.bg} ${badgePrimary.text}`}
              key={id}
            >
              {id}
            </span>
          ))}
          {components.unknown.map((id) => (
            <span
              className={`inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium ${badgeHigh.bg} ${badgeHigh.text}`}
              key={id}
              title={`${id} is no longer in the project's architecture model`}
            >
              retired: {id}
            </span>
          ))}
        </>
      )}
      {inheritedFrom}
    </div>
  );
}
