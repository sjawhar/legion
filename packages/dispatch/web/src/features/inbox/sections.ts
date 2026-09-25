import type { AskTurn, InboxRow } from "../../api/types";
import { isSnoozed } from "./snooze";

/** Where a row sits: whose turn it is, the Later band the reader deferred it into, or - in the
 *  Mine view - the Unassigned band. */
export type InboxSection = AskTurn | "later" | "unassigned";

/**
 * The Inbox's bands, top to bottom. This list and `sectionOf` below are the whole grouping
 * rule; the order WITHIN a band is the server's and the SPA re-sorts nothing, so how rows are
 * ordered is one `order by` in `listInbox` (`packages/envoy/internal/dispatch/api/inbox.go`):
 * the owning issue's priority, then whose turn it is, then the most recent activity.
 */
export const INBOX_SECTIONS = ["human", "agent", "unassigned", "later"] as const;

export const SECTION_TITLES: Record<InboxSection, string> = {
  agent: "Waiting on agents",
  human: "Waiting on you",
  later: "Later",
  unassigned: "Unassigned",
};

/** Bands the Inbox folds away until the reader opens them: rows they have already dealt with
 *  by deferring. Their heading carries the count so the fold never hides how much is in there. */
export const COLLAPSED_SECTIONS: Partial<Record<InboxSection, true>> = { later: true };

/** The viewer's own rows: asks on issues assigned to their lowercase login. */
export function isMine(row: InboxRow, viewer: string): boolean {
  return row.issue?.assignee === viewer;
}

/** Rows nobody holds: asks on unassigned issues, and every document ask (a document has no
 *  assignee). */
export function isUnassigned(row: InboxRow): boolean {
  return (row.issue?.assignee ?? null) === null;
}

/** A turn band, as opposed to one the reader's own act (an assignment, a snooze) put the row in. */
export function isTurnSection(section: InboxSection): section is AskTurn {
  return section === "human" || section === "agent";
}

/**
 * Which band a row belongs to. A snooze the reader set wins over everything: the whole point of
 * deferring an ask is that the agent answering it does not pull it back onto the list, so a row
 * inside its snooze window is in Later whatever its turn, its assignee, or how recently anyone
 * replied. A row the inbox lists without a turn belongs to no section, exactly as before the
 * views.
 */
export function sectionOf(
  row: InboxRow,
  { now, view, viewer }: { now: number; view: "everyone" | "mine"; viewer: string }
): InboxSection | undefined {
  if (isSnoozed(row, now)) return "later";
  if (view === "mine" && !isMine(row, viewer)) return "unassigned";
  return row.waiting_on;
}
