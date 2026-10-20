import type { AskTurn, InboxRow } from "../../api/types";
import { isSnoozed } from "./snooze";

/** Where a row sits: whose turn it is, or the Later band the reader deferred it into. */
export type InboxSection = AskTurn | "later";

/**
 * The Inbox's bands, top to bottom. This list and `sectionOf` below are the whole grouping
 * rule; the order WITHIN a band is the server's and the SPA re-sorts nothing, so how rows are
 * ordered is one `order by` in `listInbox` (`packages/envoy/internal/dispatch/api/inbox.go`):
 * the owning issue's priority, then whose turn it is, then the most recent activity.
 */
export const INBOX_SECTIONS = ["human", "agent", "later"] as const;

/** The bands as the reader's own list names them; `sectionTitles` below picks the record a
 *  filtered list needs. */
const SECTION_TITLES: Record<InboxSection, string> = {
  agent: "Waiting on agents",
  human: "Waiting on you",
  later: "Later",
};

/** Bands the Inbox folds away until the reader opens them: rows they have already dealt with
 *  by deferring. Their heading carries the count so the fold never hides how much is in there. */
export const COLLAPSED_SECTIONS: Partial<Record<InboxSection, true>> = { later: true };

/** The viewer's own rows: asks on issues assigned to their lowercase login. */
export function isMine(row: InboxRow, viewer: string): boolean {
  return row.issue?.assignee === viewer;
}

/** Rows nobody holds: asks on unassigned issues, and every document ask (a document has no
 *  assignee). Also what decides whether a row offers "Assign to me". */
export function isUnassigned(row: InboxRow): boolean {
  return (row.issue?.assignee ?? null) === null;
}

/** The two words the `?assignee=` vocabulary reserves beside a person's login: `unassigned`,
 *  which `inAssigneeFilter` below reads as "nobody holds this row", and `everyone`, which the
 *  route reads as the cleared filter and no selection ever holds (`parseInboxSearch` turns it
 *  into the empty selection). Both live here so the route, the filter control and the list's
 *  own rule cannot drift, and so the control can keep either word out of the people it offers. */
export const UNASSIGNED_FILTER_VALUE = "unassigned";
export const EVERYONE_FILTER_VALUE = "everyone";

/**
 * Whether a row survives the assignee filter, the one place that decides what a filtered list
 * holds. An empty selection is the cleared filter and keeps everything; otherwise a row is kept
 * when its issue's assignee is one of the selected people, or when nobody holds it
 * (`isUnassigned`, every document ask included) and `unassigned` is selected.
 */
export function inAssigneeFilter(row: InboxRow, selection: readonly string[]): boolean {
  if (selection.length === 0) return true;
  const assignee = row.issue?.assignee ?? null;
  if (assignee === null) return selection.includes(UNASSIGNED_FILTER_VALUE);
  return selection.includes(assignee);
}

/** Whether the list is the reader's own: the one selection whose bands, empty state and
 *  trigger speak in the second person. Every other selection - cleared, anybody else,
 *  `unassigned` included - is about somebody, so it is spoken of in the third. */
export function isOwnList(selection: readonly string[], viewer: string): boolean {
  return selection.length === 1 && selection[0] === viewer;
}

/**
 * The band titles for a selection. With the Unassigned band gone, another person's question now
 * lands in a turn band for the first time, and "Waiting on you" would be false there: so only
 * the reader's own list keeps the second person, and every other selection - cleared, anybody
 * else, `unassigned` included - says "Waiting on a person".
 */
export function sectionTitles(
  selection: readonly string[],
  viewer: string
): Record<InboxSection, string> {
  if (isOwnList(selection, viewer)) return SECTION_TITLES;
  return { ...SECTION_TITLES, human: "Waiting on a person" };
}

/** A turn band, as opposed to one the reader's own act (an assignment, a snooze) put the row in. */
export function isTurnSection(section: InboxSection): section is AskTurn {
  return section === "human" || section === "agent";
}

/**
 * Which band a row belongs to. A snooze the reader set wins over everything: the whole point of
 * deferring an ask is that the agent answering it does not pull it back onto the list, so a row
 * inside its snooze window is in Later whatever its turn, its assignee, or how recently anyone
 * replied. Everything else sits under whose turn it is, whoever the filter listed it for; a row
 * the inbox lists without a turn belongs to no section.
 */
export function sectionOf(row: InboxRow, now: number): InboxSection | undefined {
  if (isSnoozed(row, now)) return "later";
  return row.waiting_on;
}
