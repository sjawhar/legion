import type { ReactNode } from "react";

import type { IssuePriority } from "../api/types";
import { badgeBlocking, priorityBadge } from "../theme/classes";

export type BadgeTone = "attention" | "priority-0" | "priority-1" | "priority-2" | "priority-3";

const badgeTones: Record<BadgeTone, { bg: string; text: string }> = {
  attention: badgeBlocking,
  "priority-0": priorityBadge[0],
  "priority-1": priorityBadge[1],
  "priority-2": priorityBadge[2],
  "priority-3": priorityBadge[3],
};

export function Badge({ children, tone }: { children: ReactNode; tone: BadgeTone }): ReactNode {
  const classes = badgeTones[tone];
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold ${classes.bg} ${classes.text}`}
    >
      {children}
    </span>
  );
}

export function PriorityBadge({
  priority,
}: {
  priority: IssuePriority | null | undefined;
}): ReactNode {
  if (priority === null || priority === undefined) return null;
  return <Badge tone={`priority-${priority}`}>P{priority}</Badge>;
}

export function AttentionBadge({ count }: { count: number }): ReactNode {
  return <Badge tone="attention">{count}</Badge>;
}
