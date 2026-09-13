import type { ReactNode } from "react";

import type { IssuePriority } from "../../api/types";
import { badgeBlocking, badgeHigh, badgeLow, badgeMed } from "../../theme/classes";

const priorityBadgeClasses = [badgeBlocking, badgeHigh, badgeMed, badgeLow] as const;

export function PriorityBadge({
  priority,
}: {
  priority: IssuePriority | null | undefined;
}): ReactNode {
  if (priority === null || priority === undefined) return null;

  const badge = priorityBadgeClasses[priority];
  return (
    <span className={`rounded-full px-2 py-1 text-xs font-medium ${badge.bg} ${badge.text}`}>
      P{priority}
    </span>
  );
}
