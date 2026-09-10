import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { badgeLow, card, linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";

export function ChildrenTab({ issue }: { issue: IssueDetails }): ReactNode {
  if ((issue.children ?? []).length === 0) {
    return <p className={textMutedOnCanvas}>No child issues.</p>;
  }

  return (
    <ul className="space-y-2">
      {issue.children.map((child) => (
        <li className={`rounded-lg p-3 ${card}`} key={child.key}>
          <Link
            className={`font-medium ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: child.key, kind: "issue" })}
          >
            {child.key} · {child.title}
          </Link>
          <span className={`ml-3 rounded-full px-2 py-1 text-xs ${badgeLow.bg} ${badgeLow.text}`}>
            {child.status}
          </span>
        </li>
      ))}
    </ul>
  );
}
