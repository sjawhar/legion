import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import {
  badgeLow,
  card,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { GitHubLink } from "./GitHubLink";

export function ChildrenTab({ issue }: { issue: IssueDetails }): ReactNode {
  if ((issue.children ?? []).length === 0) {
    return <p className={textMutedOnCanvas}>No child issues.</p>;
  }

  return (
    <ul className="space-y-2">
      {issue.children.map((child) => (
        <li className={`rounded-lg p-3 ${card}`} key={child.key}>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <Link
              className={`font-medium ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: child.key, kind: "issue" })}
              {...referenceTriggerProps({ key: child.key, kind: "issue" })}
            >
              {child.key} · {child.title}
            </Link>
            <span className={`rounded-full px-2 py-1 text-xs ${badgeLow.bg} ${badgeLow.text}`}>
              {child.status}
            </span>
          </div>
          <div
            className={`mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm ${textSecondaryOnSurface}`}
          >
            <span>
              {child.subtree_done}/{child.subtree_total} done
            </span>
            <Timestamp at={child.active_at} />
            {child.external_links.map((link) => (
              <span className="flex items-center" key={link.url}>
                <GitHubLink link={link} />
              </span>
            ))}
          </div>
        </li>
      ))}
    </ul>
  );
}
