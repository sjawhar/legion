import type { ReactNode } from "react";

import type { IssueSummary } from "../../api/types";
import { badgeBlocking } from "../../theme/classes";

export function issueIsUnread(issue: IssueSummary, lastReadSequence: number): boolean {
  return issue.last_seq > lastReadSequence;
}

/** The "events newer than your last read" marker a project list row and a board card share. */
export function UnreadDot(): ReactNode {
  return <span aria-hidden className={`size-2 rounded-full ${badgeBlocking.bg}`} title="Unread" />;
}
