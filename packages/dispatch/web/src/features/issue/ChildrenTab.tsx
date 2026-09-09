import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Issue } from "../../api/types";
import { buildIssuePath } from "../refs/routes";

export function ChildrenTab({ issue }: { issue: Issue }): ReactNode {
  if ((issue.children ?? []).length === 0) {
    return <p className="text-slate-500">No child issues.</p>;
  }

  return (
    <ul className="space-y-2">
      {issue.children?.map((child) => (
        <li className="rounded-lg border border-slate-200 bg-white p-3" key={child.key}>
          <Link
            className="font-medium text-sky-700 hover:text-sky-900"
            to={buildIssuePath({ key: child.key, kind: "issue" })}
          >
            {child.key} · {child.title}
          </Link>
          <span className="ml-3 rounded-full bg-slate-100 px-2 py-1 text-xs text-slate-600">
            {child.status}
          </span>
        </li>
      ))}
    </ul>
  );
}
