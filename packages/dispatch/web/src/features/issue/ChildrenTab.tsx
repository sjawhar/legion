import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { StatusPill } from "../../components/Pill";
import { card, linkHoverText, linkText, textSecondaryOnSurface } from "../../theme/classes";
import { statusText } from "../project/board-model";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { GitHubLink } from "./GitHubLink";

export function ChildrenTab({ issue }: { issue: IssueDetails }): ReactNode {
  if ((issue.children ?? []).length === 0) {
    return <EmptyState label="Children empty state" message="No child issues" />;
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
            {/* The same pill the issue header shows, with the same lifecycle label: a reader
                two inches below `In progress` must not meet `in_progress`. */}
            <StatusPill>{statusText(child.status)}</StatusPill>
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
