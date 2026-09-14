import type { ReactNode } from "react";

import { badgeBlocking } from "../theme/classes";

/** The open-ask count on board cards and list rows. Priority uses the same tag shape but is
 *  editable everywhere it appears, so it lives in `features/issue/PriorityControl`. */
export function AttentionBadge({ count }: { count: number }): ReactNode {
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold ${badgeBlocking.bg} ${badgeBlocking.text}`}
    >
      {count}
    </span>
  );
}
